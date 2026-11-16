// Package logstream applies Kubernetes log options to timestamped provider events.
package logstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

const MaxLineBytes = 1 << 20
const maxTailBytes = 4 << 20

// Bound memory and long-lived upstream connections across all log backends.
var activeStreams = make(chan struct{}, 16)

type Event struct {
	Timestamp time.Time
	Line      string
}
type Producer func(context.Context, func(Event) error) error
type Timing struct {
	SnapshotIdle, SnapshotMax time.Duration
	Finite                    bool
}

// Open owns producer's context. Producers must unblock when it is cancelled.
func Open(parent context.Context, o core.LogOptions, produce Producer, t Timing) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, err
	}
	if o.Tail != nil && *o.Tail == 0 && !o.Follow {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if produce == nil {
		return nil, fmt.Errorf("missing log producer")
	}
	select {
	case activeStreams <- struct{}{}:
	default:
		return nil, fmt.Errorf("concurrent log stream limit reached")
	}
	if t.SnapshotIdle <= 0 {
		t.SnapshotIdle = time.Second
	}
	if t.SnapshotMax <= 0 {
		t.SnapshotMax = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(parent)
	pr, pw := io.Pipe()
	events := make(chan Event, 1)
	result := make(chan error, 1)
	go func() {
		err := produce(ctx, func(e Event) error {
			if len(e.Line) > MaxLineBytes {
				return fmt.Errorf("log record exceeds %d bytes", MaxLineBytes)
			}
			select {
			case events <- e:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		result <- err
		close(events)
	}()
	go func() {
		defer cancel()
		defer func() { <-activeStreams }()
		err := copyEvents(ctx, pw, events, result, o, t)
		_ = pw.CloseWithError(err)
	}()
	go func() { <-ctx.Done(); _ = pw.CloseWithError(ctx.Err()) }()
	return &reader{ReadCloser: pr, cancel: cancel}, nil
}

type reader struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *reader) Close() error { r.once.Do(r.cancel); return r.ReadCloser.Close() }

func copyEvents(ctx context.Context, w io.Writer, events <-chan Event, result <-chan error, o core.LogOptions, t Timing) error {
	replay := true
	buffering := o.Tail != nil && *o.Tail > 0
	var ring []string
	var size int
	remaining := o.LimitBytes
	var snapshot bytes.Buffer
	write := func(s string) error {
		if o.LimitBytes > 0 && int64(len(s)) > remaining {
			s = s[:remaining]
		}
		var n int
		var err error
		if !o.Follow {
			if snapshot.Len()+len(s) > maxTailBytes {
				return fmt.Errorf("snapshot exceeds %d byte buffer; use tail or limitBytes", maxTailBytes)
			}
			n, err = snapshot.WriteString(s)
		} else {
			n, err = io.WriteString(w, s)
		}
		remaining -= int64(n)
		if err != nil {
			return err
		}
		if o.LimitBytes > 0 && remaining == 0 {
			return io.EOF
		}
		return nil
	}
	flush := func() error {
		for _, s := range ring {
			if err := write(s); err != nil {
				return err
			}
		}
		ring = nil
		size = 0
		buffering = false
		return nil
	}
	var idle *time.Timer
	var idleC, maxC <-chan time.Time
	bounded := !t.Finite && (!o.Follow || buffering)
	if bounded {
		ceiling := time.NewTimer(t.SnapshotMax)
		defer ceiling.Stop()
		maxC = ceiling.C
	}
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()
	endReplay := func() (bool, error) {
		replay = false
		idleC = nil
		maxC = nil
		if err := flush(); err != nil {
			return true, err
		}
		return !o.Follow, nil
	}
	normalize := func(err error) error {
		if err == nil || err == io.EOF {
			if !o.Follow {
				_, err = w.Write(snapshot.Bytes())
				return err
			}
			return nil
		}
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idleC:
			// Avoid selecting a stale silence timer over already queued output.
			if len(events) > 0 {
				idle.Reset(t.SnapshotIdle)
				continue
			}
			done, err := endReplay()
			if done || err != nil {
				return normalize(err)
			}
		case <-maxC:
			done, err := endReplay()
			if done || err != nil {
				return normalize(err)
			}
		case e, ok := <-events:
			if !ok {
				if err := <-result; err != nil {
					return err
				}
				return normalize(flush())
			}
			if bounded && replay {
				if idle == nil {
					idle = time.NewTimer(t.SnapshotIdle)
				} else {
					idle.Reset(t.SnapshotIdle)
				}
				idleC = idle.C
			}
			if !o.SinceTime.IsZero() && e.Timestamp.Before(o.SinceTime) {
				continue
			}
			s := strings.TrimSuffix(e.Line, "\n") + "\n"
			if o.Timestamps {
				if e.Timestamp.IsZero() {
					return fmt.Errorf("log record has no timestamp")
				}
				s = e.Timestamp.UTC().Format(time.RFC3339Nano) + " " + s
			}
			if buffering {
				ring = append(ring, s)
				size += len(s)
				if len(ring) > *o.Tail {
					size -= len(ring[0])
					ring = ring[1:]
				}
				if size > maxTailBytes {
					return fmt.Errorf("requested tail exceeds %d byte buffer", maxTailBytes)
				}
			} else {
				if err := write(s); err != nil {
					return normalize(err)
				}
			}
		}
	}
}
