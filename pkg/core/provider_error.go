package core

import (
	"context"
	"errors"
	"fmt"
)

type ErrorClass string

const (
	ErrorClassUnknown         ErrorClass = "Unknown"
	ErrorClassUnsupportedSpec ErrorClass = "UnsupportedSpec"
	ErrorClassAuth            ErrorClass = "Auth"
	ErrorClassCapacity        ErrorClass = "Capacity"
	ErrorClassQuota           ErrorClass = "Quota"
	ErrorClassRateLimited     ErrorClass = "RateLimited"
	ErrorClassTransient       ErrorClass = "Transient"
	ErrorClassTimeout         ErrorClass = "Timeout"
	ErrorClassConflict        ErrorClass = "Conflict"
	ErrorClassRemoteNotFound  ErrorClass = "RemoteNotFound"
	ErrorClassDeleteFailure   ErrorClass = "DeleteFailure"
)

type ProviderError struct {
	Provider   string
	Operation  string
	StatusCode int
	Class      ErrorClass
	Code       string
	Message    string
	Err        error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return ""
	}
	message := e.Message
	if message == "" && e.Err != nil {
		message = e.Err.Error()
	}
	if message == "" {
		message = string(e.Class)
	}
	prefix := e.Provider
	if e.Operation != "" {
		if prefix != "" {
			prefix += " "
		}
		prefix += e.Operation
	}
	if e.StatusCode > 0 {
		if prefix != "" {
			prefix += " "
		}
		prefix += fmt.Sprintf("status %d", e.StatusCode)
	}
	if prefix == "" {
		return message
	}
	return prefix + ": " + message
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ""
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr.Class != "" {
		return providerErr.Class
	}
	var unsupported *UnsupportedSpecError
	if errors.As(err, &unsupported) {
		return ErrorClassUnsupportedSpec
	}
	var policy *PolicyViolationError
	if errors.As(err, &policy) {
		return ErrorClassUnsupportedSpec
	}
	if errors.Is(err, ErrRemoteNotFound) {
		return ErrorClassRemoteNotFound
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorClassTimeout
	}
	if errors.Is(err, context.Canceled) {
		return ErrorClassTimeout
	}
	return ErrorClassUnknown
}

func IsRetryable(err error) bool {
	return IsRetryableClass(ClassifyError(err))
}

func AllowsFallback(err error) bool {
	return AllowsFallbackClass(ClassifyError(err))
}

func IsRetryableClass(class ErrorClass) bool {
	switch class {
	case ErrorClassRateLimited, ErrorClassTransient, ErrorClassTimeout, ErrorClassConflict:
		return true
	default:
		return false
	}
}

func AllowsFallbackClass(class ErrorClass) bool {
	switch class {
	case ErrorClassCapacity, ErrorClassRateLimited, ErrorClassTransient, ErrorClassTimeout:
		return true
	default:
		return false
	}
}
