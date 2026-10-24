package browserclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type ErrorKind string

const (
	ErrorInvalid        ErrorKind = "invalid"
	ErrorAuthentication ErrorKind = "authentication"
	ErrorProtocol       ErrorKind = "protocol"
	ErrorLimit          ErrorKind = "limit"
	ErrorBusy           ErrorKind = "busy"
	ErrorCancelled      ErrorKind = "cancelled"
	ErrorTimeout        ErrorKind = "timeout"
	ErrorUnavailable    ErrorKind = "unavailable"
	ErrorExecutor       ErrorKind = "executor"
	ErrorClosed         ErrorKind = "closed"
)

var (
	ErrInvalid        = errors.New("invalid browser client request")
	ErrAuthentication = errors.New("browser peer authentication failed")
	ErrProtocol       = errors.New("browser worker protocol violation")
	ErrLimit          = errors.New("browser worker limit exceeded")
	ErrBusy           = errors.New("browser worker is busy")
	ErrCancelled      = errors.New("browser request cancelled")
	ErrTimeout        = errors.New("browser request timed out")
	ErrUnavailable    = errors.New("browser worker unavailable")
	ErrExecutor       = errors.New("browser executor failed")
	ErrClosed         = errors.New("browser client closed")
)

type Error struct {
	Kind          ErrorKind
	Operation     Operation
	CorrelationID string
	Code          string
	Message       string
	Remote        bool
	Cause         error
}

func (err *Error) Error() string {
	if err == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if err.Operation != "" {
		parts = append(parts, "browser "+string(err.Operation))
	} else {
		parts = append(parts, "browser client")
	}
	if err.Code != "" {
		parts = append(parts, "["+Redact(err.Code)+"]")
	}
	message := strings.TrimSpace(Redact(err.Message))
	if message == "" && err.Cause != nil {
		message = strings.TrimSpace(Redact(err.Cause.Error()))
	}
	if message == "" {
		message = string(err.Kind)
	}
	return strings.Join(parts, " ") + ": " + message
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (err *Error) Is(target error) bool {
	if err == nil {
		return false
	}
	return target == sentinelForKind(err.Kind)
}

func KindOf(err error) ErrorKind {
	var clientError *Error
	if errors.As(err, &clientError) {
		return clientError.Kind
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ErrorCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorTimeout
	default:
		return ErrorUnavailable
	}
}

func sentinelForKind(kind ErrorKind) error {
	switch kind {
	case ErrorInvalid:
		return ErrInvalid
	case ErrorAuthentication:
		return ErrAuthentication
	case ErrorProtocol:
		return ErrProtocol
	case ErrorLimit:
		return ErrLimit
	case ErrorBusy:
		return ErrBusy
	case ErrorCancelled:
		return ErrCancelled
	case ErrorTimeout:
		return ErrTimeout
	case ErrorUnavailable:
		return ErrUnavailable
	case ErrorExecutor:
		return ErrExecutor
	case ErrorClosed:
		return ErrClosed
	default:
		return nil
	}
}

func clientError(kind ErrorKind, operation Operation, correlationID, message string, cause error) error {
	return &Error{
		Kind: kind, Operation: operation, CorrelationID: correlationID,
		Message: Redact(message), Cause: cause,
	}
}

func contextError(operation Operation, correlationID string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return clientError(ErrorTimeout, operation, correlationID, "request deadline exceeded", err)
	}
	return clientError(ErrorCancelled, operation, correlationID, "request context cancelled", err)
}

func remoteError(operation Operation, correlationID string, value *wireError) error {
	if value == nil {
		return clientError(ErrorProtocol, operation, correlationID, "missing remote error", ErrProtocol)
	}
	kind := ErrorExecutor
	switch value.Code {
	case "busy":
		kind = ErrorBusy
	case "cancelled":
		kind = ErrorCancelled
	case "browser_unavailable":
		kind = ErrorUnavailable
	case "result_rejected":
		kind = ErrorLimit
	case "correlation_rejected", "operation_rejected", "request_rejected":
		kind = ErrorInvalid
	case "executor_failed":
		kind = ErrorExecutor
	}
	return &Error{
		Kind: kind, Operation: operation, CorrelationID: correlationID,
		Code: value.Code, Message: Redact(value.Message), Remote: true,
		Cause: sentinelForKind(kind),
	}
}

func wrapUnavailable(operation Operation, correlationID, action string, err error) error {
	return clientError(
		ErrorUnavailable,
		operation,
		correlationID,
		fmt.Sprintf("%s failed", action),
		err,
	)
}
