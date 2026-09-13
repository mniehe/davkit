package carddav

import (
	"errors"
	"fmt"
)

// Sentinel errors a backend returns. Match them with errors.Is; anything else
// becomes a 500.
var (
	ErrNotFound           = errors.New("carddav: not found")
	ErrParentNotFound     = errors.New("carddav: parent not found")
	ErrForbidden          = errors.New("carddav: operation not permitted")
	ErrPreconditionFailed = errors.New("carddav: precondition failed")
	ErrHistoryTooOld      = errors.New("carddav: change history does not reach that revision")
	ErrUnauthorized       = errors.New("carddav: not authenticated")
	ErrAlreadyExists      = errors.New("carddav: already exists")
)

// DuplicateContentIDError reports that another item in the address book already
// calls itself by the content ID being stored.
type DuplicateContentIDError struct {
	Existing Segment
}

func (e *DuplicateContentIDError) Error() string {
	return fmt.Sprintf("carddav: content ID already used by %q", e.Existing)
}

// InvalidContentError reports content that does not parse.
type InvalidContentError struct {
	Err error
}

func (e *InvalidContentError) Error() string {
	return fmt.Sprintf("carddav: invalid content: %v", e.Err)
}

func (e *InvalidContentError) Unwrap() error { return e.Err }

// QuotaExceededError reports a write that would take the address book over a limit
// the backend enforces.
type QuotaExceededError struct {
	Limit int64
}

func (e *QuotaExceededError) Error() string {
	return fmt.Sprintf("carddav: quota of %d bytes exceeded", e.Limit)
}
