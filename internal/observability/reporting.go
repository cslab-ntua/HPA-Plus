package observability

import (
	"context"
	"errors"
	"strings"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// PublicError retains error identity for retry/safety decisions while exposing
// only fixed categories to framework loggers. Never format the underlying error.
type publicError struct{ cause error }

func (e *publicError) Error() string { return ErrorReason(e.cause) }
func (e *publicError) Unwrap() error { return e.cause }

func PublicError(err error) error {
	if err == nil {
		return nil
	}
	return &publicError{cause: err}
}

func ErrorReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return "timeout"
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case apierrors.IsConflict(err):
		return "conflict"
	case apierrors.IsNotFound(err):
		return "not_found"
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return "invalid"
	case apierrors.IsTooManyRequests(err):
		return "throttled"
	case apierrors.IsServiceUnavailable(err):
		return "unavailable"
	default:
		return "operation_failed"
	}
}

// Field bounds user-provided identity fields and removes control characters.
// It is not a redactor: arbitrary configuration/error text must never be passed.
func Field(value string) string {
	var result strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) {
			continue
		}
		if result.Len()+len(string(char)) > 256 {
			break
		}
		result.WriteRune(char)
	}
	return result.String()
}
