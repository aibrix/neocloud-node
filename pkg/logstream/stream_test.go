package logstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestLogsFilterBeforeTailAndFormat(t *testing.T) {
	n := 2
	since := time.Date(2026, 11, 14, 0, 0, 1, 0, time.UTC)
	r, err := Open(context.Background(), core.LogOptions{Tail: &n, SinceTime: since, Timestamps: true}, func(ctx context.Context, emit func(Event) error) error {
		for i, line := range []string{"old", "one", "two", "three"} {
			if err := emit(Event{Timestamp: since.Add(time.Duration(i-1) * time.Second), Line: line}); err != nil {
				return err
			}
		}
		return nil
	}, Timing{Finite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "2026-11-14T00:00:02Z two\n2026-11-14T00:00:03Z three\n" {
		t.Fatalf("logs=%q", b)
	}
}

func TestLogsLimitAndExplicitZeroTail(t *testing.T) {
	zero := 0
	for _, tt := range []struct {
		name  string
		opts  core.LogOptions
		want  string
		calls bool
	}{
		{"bytes", core.LogOptions{LimitBytes: 3}, "hel", true},
		{"zero", core.LogOptions{Tail: &zero}, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			r, err := Open(context.Background(), tt.opts, func(ctx context.Context, emit func(Event) error) error {
				called = true
				return emit(Event{Line: "hello"})
			}, Timing{})
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(b) != tt.want || called != tt.calls {
				t.Fatalf("logs=%q err=%v called=%t", b, err, called)
			}
		})
	}
}

func TestLogsFollowSurvivesSilenceAndCloseCancelsProducer(t *testing.T) {
	stopped := make(chan struct{})
	r, err := Open(context.Background(), core.LogOptions{Follow: true}, func(ctx context.Context, emit func(Event) error) error {
		defer close(stopped)
		if err := emit(Event{Line: "first"}); err != nil {
			return err
		}
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := emit(Event{Line: "later"}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}, Timing{SnapshotIdle: time.Millisecond, SnapshotMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len("first\nlater\n"))
	_, err = io.ReadFull(r, b)
	if err != nil || string(b) != "first\nlater\n" {
		t.Fatalf("logs=%q err=%v", b, err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { _ = r.Close() })
	}
	wg.Wait()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("producer leaked")
	}
}

func TestLogsSnapshotUsesIdleOrCeiling(t *testing.T) {
	for _, chatty := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "ceiling"}[chatty], func(t *testing.T) {
			r, err := Open(context.Background(), core.LogOptions{}, func(ctx context.Context, emit func(Event) error) error {
				if err := emit(Event{Line: "ready"}); err != nil {
					return err
				}
				if !chatty {
					<-ctx.Done()
					return ctx.Err()
				}
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Millisecond):
						if err := emit(Event{Line: "more"}); err != nil {
							return err
						}
					}
				}
			}, Timing{SnapshotIdle: 10 * time.Millisecond, SnapshotMax: 40 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			b, err := io.ReadAll(r)
			if err != nil || !strings.HasPrefix(string(b), "ready\n") {
				t.Fatalf("logs=%q err=%v", b, err)
			}
		})
	}
}

func TestLogsPropagatesFailureAndRejectsUnsupportedOptions(t *testing.T) {
	r, err := Open(context.Background(), core.LogOptions{}, func(context.Context, func(Event) error) error { return errors.New("upstream unavailable") }, Timing{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("error=%v", err)
	}
	for _, o := range []core.LogOptions{{Previous: true}, {LimitBytes: -1}} {
		if _, err := Open(context.Background(), o, nil, Timing{}); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
}

func TestLogsRejectsOversizedRecords(t *testing.T) {
	r, err := Open(context.Background(), core.LogOptions{}, func(ctx context.Context, emit func(Event) error) error {
		return emit(Event{Line: strings.Repeat("x", (1<<20)+1)})
	}, Timing{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, err = io.ReadAll(r)
	if err == nil {
		t.Fatal("oversized log accepted")
	}
}

func TestLogsBoundsConcurrentStreamsAndReleasesSlots(t *testing.T) {
	var readers []io.ReadCloser
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	for range 16 {
		r, err := Open(context.Background(), core.LogOptions{Follow: true}, func(ctx context.Context, emit func(Event) error) error { <-ctx.Done(); return ctx.Err() }, Timing{})
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, r)
	}
	if r, err := Open(context.Background(), core.LogOptions{Follow: true}, func(context.Context, func(Event) error) error { return nil }, Timing{}); err == nil {
		r.Close()
		t.Fatal("concurrent stream limit not enforced")
	}
	for _, r := range readers {
		r.Close()
	}
	readers = nil
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r, err := Open(context.Background(), core.LogOptions{}, func(context.Context, func(Event) error) error { return nil }, Timing{})
		if err == nil {
			r.Close()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("stream slots leaked after close")
}

func TestLogsFiniteSnapshotDoesNotUseIdleTimeout(t *testing.T) {
	r, err := Open(context.Background(), core.LogOptions{}, func(ctx context.Context, emit func(Event) error) error {
		if err := emit(Event{Line: "first"}); err != nil {
			return err
		}
		time.Sleep(20 * time.Millisecond)
		return emit(Event{Line: "second"})
	}, Timing{Finite: true, SnapshotIdle: time.Millisecond, SnapshotMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil || string(out) != "first\nsecond\n" {
		t.Fatalf("finite output=%q err=%v", out, err)
	}
}

func TestLogsSnapshotDoesNotCommitPartialOutputBeforeSourceFailure(t *testing.T) {
	r, err := Open(context.Background(), core.LogOptions{}, func(ctx context.Context, emit func(Event) error) error {
		if err := emit(Event{Line: "partial"}); err != nil {
			return err
		}
		return errors.New("source failed")
	}, Timing{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err == nil || len(b) != 0 {
		t.Fatalf("snapshot exposed partial output: %q err=%v", b, err)
	}
}
