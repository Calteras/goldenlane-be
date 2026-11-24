// pkg/httpx/errors.go
package httpserver

import (
	"fmt"
)

type AppError struct {
	Code    int    // HTTP status code
	Message string // user-facing message
	Err     error  // underlying error (optional)
	// Reason is an optional machine-readable code clients can branch on, such as
	// "password_change_required". It is rendered as "code" in the JSON envelope.
	Reason string
	// Data is optional structured detail rendered as "data" (for example which lines of an
	// order cannot be ordered, or the new price).
	Data any
}

// WithReason sets the machine-readable code and returns the error.
func (e *AppError) WithReason(reason string) *AppError {
	e.Reason = reason
	return e
}

// WithData attaches structured detail to the error body and returns the error.
func (e *AppError) WithData(data any) *AppError {
	e.Data = data

	return e
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("code=%d message=%q err=%s", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("code=%d message=%q", e.Code, e.Message)
}

// Helper constructors for HTTP error statuses
func NewBadRequest(msg string, err error) *AppError {
	return &AppError{Code: 400, Message: msg, Err: err}
}

func NewUnauthorized(msg string, err error) *AppError {
	return &AppError{Code: 401, Message: msg, Err: err}
}

func NewForbidden(msg string, err error) *AppError {
	return &AppError{Code: 403, Message: msg, Err: err}
}

func NewNotFound(msg string, err error) *AppError {
	return &AppError{Code: 404, Message: msg, Err: err}
}

func NewMethodNotAllowed(msg string, err error) *AppError {
	return &AppError{Code: 405, Message: msg, Err: err}
}

func NewConflict(msg string, err error) *AppError {
	return &AppError{Code: 409, Message: msg, Err: err}
}

func NewUnprocessableEntity(msg string, err error) *AppError {
	return &AppError{Code: 422, Message: msg, Err: err}
}

func NewTooManyRequests(msg string, err error) *AppError {
	return &AppError{Code: 429, Message: msg, Err: err}
}

func NewInternalServerError(msg string, err error) *AppError {
	return &AppError{Code: 500, Message: msg, Err: err}
}

func NewBadGateway(msg string, err error) *AppError {
	return &AppError{Code: 502, Message: msg, Err: err}
}

func NewServiceUnavailable(msg string, err error) *AppError {
	return &AppError{Code: 503, Message: msg, Err: err}
}
