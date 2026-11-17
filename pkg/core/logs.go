package core

import (
	"context"
	"fmt"
	"io"
	"time"
)

// LogTarget is resolved from the current Pod UID and its durable binding.
type LogTarget struct{ Namespace, WorkloadUID, RemoteID, ContainerName string }

// LogOptions retains nil Tail (all retained history) separately from explicit 0.
type LogOptions struct {
	Tail                         *int
	SinceTime                    time.Time
	Follow, Timestamps, Previous bool
	LimitBytes                   int64
}

func (o LogOptions) Validate() error {
	if o.Previous {
		return fmt.Errorf("previous container logs are not supported")
	}
	if o.LimitBytes < 0 {
		return fmt.Errorf("limitBytes must not be negative")
	}
	if o.Tail != nil && (*o.Tail < 0 || *o.Tail > 5000) {
		return fmt.Errorf("tail must be between 0 and 5000")
	}
	return nil
}

// ProviderLogReader is optional; lifecycle-only adapters need not implement it.
type ProviderLogReader interface {
	OpenLogs(context.Context, LogTarget, LogOptions) (io.ReadCloser, error)
}
