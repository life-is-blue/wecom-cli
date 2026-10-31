// Package core holds the pure write-rule validation of openetl. It never touches SQL.
package core

import "fmt"

// Status values mirror the AIP-193 status strings of the OpenAPI Error schema.
const (
	StatusInvalidArgument    = "INVALID_ARGUMENT"
	StatusUnauthenticated    = "UNAUTHENTICATED"
	StatusPermissionDenied   = "PERMISSION_DENIED"
	StatusNotFound           = "NOT_FOUND"
	StatusAlreadyExists      = "ALREADY_EXISTS"
	StatusAborted            = "ABORTED"
	StatusFailedPrecondition = "FAILED_PRECONDITION"
	StatusResourceExhausted  = "RESOURCE_EXHAUSTED"
	StatusUnimplemented      = "UNIMPLEMENTED"
	StatusInternal           = "INTERNAL"
)

// Error is a structured, client-visible error (status + ErrorInfo reason).
type Error struct {
	Status   string
	Reason   string
	Message  string
	Metadata map[string]string
}

func (e *Error) Error() string { return fmt.Sprintf("%s/%s: %s", e.Status, e.Reason, e.Message) }

// NewError builds an *Error.
func NewError(status, reason, format string, args ...any) *Error {
	return &Error{Status: status, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// With adds a metadata pair and returns the error for chaining.
func (e *Error) With(k, v string) *Error {
	if e.Metadata == nil {
		e.Metadata = map[string]string{}
	}
	e.Metadata[k] = v
	return e
}
