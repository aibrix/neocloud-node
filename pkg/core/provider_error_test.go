package core

import (
	"context"
	"errors"
	"testing"
)

func TestProviderErrorClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		want     ErrorClass
		retry    bool
		fallback bool
	}{
		{
			name:     "rate limited",
			err:      &ProviderError{Class: ErrorClassRateLimited},
			want:     ErrorClassRateLimited,
			retry:    true,
			fallback: true,
		},
		{
			name:     "capacity",
			err:      &ProviderError{Class: ErrorClassCapacity},
			want:     ErrorClassCapacity,
			retry:    false,
			fallback: true,
		},
		{
			name:     "auth",
			err:      &ProviderError{Class: ErrorClassAuth},
			want:     ErrorClassAuth,
			retry:    false,
			fallback: false,
		},
		{
			name:     "timeout",
			err:      context.DeadlineExceeded,
			want:     ErrorClassTimeout,
			retry:    true,
			fallback: true,
		},
		{
			name:     "remote not found",
			err:      ErrRemoteNotFound,
			want:     ErrorClassRemoteNotFound,
			retry:    false,
			fallback: false,
		},
		{
			name:     "unknown",
			err:      errors.New("provider failed"),
			want:     ErrorClassUnknown,
			retry:    false,
			fallback: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := ClassifyError(tt.err); got != tt.want {
				t.Fatalf("ClassifyError() = %s, want %s", got, tt.want)
			}
			if got := IsRetryable(tt.err); got != tt.retry {
				t.Fatalf("IsRetryable() = %t, want %t", got, tt.retry)
			}
			if got := AllowsFallback(tt.err); got != tt.fallback {
				t.Fatalf("AllowsFallback() = %t, want %t", got, tt.fallback)
			}
		})
	}
}

func TestProviderErrorWrapsRemoteNotFound(t *testing.T) {
	t.Parallel()

	err := &ProviderError{
		Provider:  "runpod",
		Operation: "get",
		Class:     ErrorClassRemoteNotFound,
		Err:       ErrRemoteNotFound,
	}
	if !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("errors.Is(provider error, ErrRemoteNotFound) = false, want true")
	}
}
