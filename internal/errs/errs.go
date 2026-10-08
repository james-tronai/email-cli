// Package errs defines the CLI's typed errors and their exit codes.
//
// Exit codes are part of the contract with the agent:
//
//	0 success
//	1 internal error
//	2 usage error (bad flags/args, unsupported capability)
//	3 authentication failed or re-login required
//	4 denied by policy
//	5 provider error (carries the provider's own error code)
//	6 timeout
package errs

import (
	"context"
	"errors"
	"fmt"
)

const (
	ExitOK       = 0
	ExitInternal = 1
	ExitUsage    = 2
	ExitAuth     = 3
	ExitPolicy   = 4
	ExitProvider = 5
	ExitTimeout  = 6
)

// Error is an error with a stable exit code and machine-readable code.
type Error struct {
	Exit         int    `json:"exit_code"`
	Code         string `json:"code"`
	Message      string `json:"message"`
	ProviderCode string `json:"provider_code,omitempty"`
	Err          error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Err != nil && e.Message == "" {
		return e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func newf(exit int, code, format string, args ...any) *Error {
	return &Error{Exit: exit, Code: code, Message: fmt.Sprintf(format, args...)}
}

func Usage(format string, args ...any) *Error {
	return newf(ExitUsage, "usage", format, args...)
}

func Unsupported(format string, args ...any) *Error {
	return newf(ExitUsage, "unsupported", format, args...)
}

func Auth(format string, args ...any) *Error {
	return newf(ExitAuth, "auth_required", format, args...)
}

func Policy(format string, args ...any) *Error {
	return newf(ExitPolicy, "policy_denied", format, args...)
}

// Provider wraps an upstream API failure. providerCode is the provider's own
// error code (e.g. Graph "ErrorItemNotFound", Gmail "notFound").
func Provider(providerCode string, err error) *Error {
	return &Error{Exit: ExitProvider, Code: "provider_error", Message: err.Error(), ProviderCode: providerCode, Err: err}
}

func Timeout(err error) *Error {
	return &Error{Exit: ExitTimeout, Code: "timeout", Message: "operation timed out", Err: err}
}

// From converts any error into an *Error, classifying context deadlines as
// timeouts and everything unknown as internal.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Timeout(err)
	}
	return &Error{Exit: ExitInternal, Code: "internal", Message: err.Error(), Err: err}
}

func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	return From(err).Exit
}
