package runner

import (
	"context"
	"errors"
	"fmt"
	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
	"io"
	"strings"
	"testing"
	"time"
)

var logTarget = core.LogTarget{Namespace: "ns", WorkloadUID: "uid", RemoteID: "vm", ContainerName: "main"}

func batch(lines ...string) LogBatch {
	b := LogBatch{WorkloadID: "ns/uid", Generation: "launch", FirstSequence: 1, ReplayComplete: true}
	for i, s := range lines {
		b.Records = append(b.Records, LogRecord{Sequence: uint64(i + 1), Timestamp: time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC), Line: s})
	}
	return b
}
func acceptReplay(t *testing.T, s *LogStore, b LogBatch) LogBatch {
	t.Helper()
	a, e := s.Accept(logTarget, b)
	if e != nil {
		t.Fatal(e)
	}
	if !a.ReplayRequired {
		t.Fatal("fresh store must request replay")
	}
	b.Epoch = a.Epoch
	if _, e = s.Accept(logTarget, b); e != nil {
		t.Fatal(e)
	}
	return b
}

func TestLogsStaleSnapshotFailsButCompletedHistoryRemainsReadable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lines    []string
		complete bool
	}{{"empty", nil, false}, {"nonempty", []string{"old"}, false}, {"completed", []string{"done"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLogStore(LogStoreConfig{ReplayTimeout: time.Second})
			b := batch(tc.lines...)
			b.Complete = tc.complete
			acceptReplay(t, s, b)
			s.mu.Lock()
			s.states["ns/uid"].updated = time.Now().Add(-time.Minute)
			s.mu.Unlock()
			r, err := s.OpenLogs(context.Background(), logTarget, core.LogOptions{})
			var data []byte
			if err == nil {
				defer r.Close()
				data, err = io.ReadAll(r)
			}
			if tc.complete {
				if err != nil || string(data) != "done\n" {
					t.Fatalf("completed logs=%q err=%v", data, err)
				}
			} else if err == nil {
				t.Fatalf("stale snapshot succeeded: %q", data)
			}
		})
	}
}
func TestLogsReplayDedupAndComplete(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := acceptReplay(t, s, batch("one", "two"))
	b.Complete = true
	if _, e := s.Accept(logTarget, b); e != nil {
		t.Fatal(e)
	}
	for _, follow := range []bool{false, true} {
		r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{Follow: follow})
		if e != nil {
			t.Fatal(e)
		}
		out, e := io.ReadAll(r)
		r.Close()
		if e != nil || string(out) != "one\ntwo\n" {
			t.Fatalf("%q %v", out, e)
		}
	}
	fresh := NewLogStore(LogStoreConfig{})
	acceptReplay(t, fresh, b)
	r, e := fresh.OpenLogs(context.Background(), logTarget, core.LogOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	out, e := io.ReadAll(r)
	if e != nil || string(out) != "one\ntwo\n" {
		t.Fatalf("restart %q %v", out, e)
	}
}
func TestLogsRejectIdentityGapsAndHistoryLoss(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxBytesPerWorkload: 140})
	b := acceptReplay(t, s, batch("one", "two", "three"))
	r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{})
	if r != nil {
		r.Close()
	}
	if !errors.Is(e, ErrLogHistoryLost) {
		t.Fatalf("history error %v", e)
	}
	b.Generation = "foreign"
	if _, e = s.Accept(logTarget, b); e == nil {
		t.Fatal("foreign generation accepted")
	}
	b.Generation = "launch"
	b.Records = []LogRecord{{Sequence: 9, Timestamp: time.Now(), Line: "gap"}}
	if _, e = s.Accept(logTarget, b); e == nil {
		t.Fatal("gap accepted")
	}
	other := logTarget
	other.RemoteID = "other"
	if _, e = s.Accept(other, b); e == nil {
		t.Fatal("foreign remote accepted")
	}
}
func TestLogsCancelAndUnavailable(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	if _, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{}); e == nil {
		t.Fatal("missing logs succeeded")
	}
	acceptReplay(t, s, batch())
	ctx, cancel := context.WithCancel(context.Background())
	r, e := s.OpenLogs(ctx, logTarget, core.LogOptions{Follow: true})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	_, e = io.ReadAll(r)
	r.Close()
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel %v", e)
	}
}
func TestLogSignature(t *testing.T) {
	body := []byte(`{"workloadID":"ns/uid"}`)
	ts := "1794614400"
	sig := SignLogBatch("secret", "ns/uid", ts, body)
	now := time.Unix(1794614400, 0)
	if e := VerifyLogBatch("secret", "ns/uid", ts, sig, body, now); e != nil {
		t.Fatal(e)
	}
	if e := VerifyLogBatch("secret", "other", ts, sig, body, now); e == nil {
		t.Fatal("cross workload signature accepted")
	}
	if e := VerifyLogBatch("secret", "ns/uid", ts, sig, body, now.Add(time.Hour)); e == nil {
		t.Fatal("expired signature accepted")
	}
	if strings.Contains(DeriveLogKey("secret", "ns/uid"), "secret") {
		t.Fatal("not derived")
	}
}

func TestLogsBoundsTailAndTransportFailure(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxWorkloads: 1, MaxStreams: 1, ReplayTimeout: 20 * time.Millisecond, MaxBytesPerWorkload: 140})
	b := acceptReplay(t, s, batch("one", "two", "three"))
	tail := 1
	r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{Tail: &tail})
	if e != nil {
		t.Fatal(e)
	}
	out, e := io.ReadAll(r)
	r.Close()
	if e != nil || string(out) != "three\n" {
		t.Fatalf("tail %q %v", out, e)
	}
	b.Records = nil
	b.ReplayComplete = true
	b.Epoch = "wrong"
	a, e := s.Accept(logTarget, b)
	if e != nil || !a.ReplayRequired {
		t.Fatalf("epoch reset %v %v", a, e)
	}
	s.Forget(logTarget)
	s = NewLogStore(LogStoreConfig{MaxStreams: 1, ReplayTimeout: 20 * time.Millisecond})
	acceptReplay(t, s, batch())
	r, e = s.OpenLogs(context.Background(), logTarget, core.LogOptions{Follow: true})
	if e != nil {
		t.Fatal(e)
	}
	if extra, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{Follow: true}); e == nil {
		extra.Close()
		t.Fatal("stream limit ignored")
	}
	_, e = io.ReadAll(r)
	r.Close()
	if e == nil || !strings.Contains(e.Error(), "timed out") {
		t.Fatalf("transport error %v", e)
	}
}
func TestLogsRejectOversizedBatchAndTruncatedReplay(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := batch(strings.Repeat("x", (1<<20)+1))
	if _, e := s.Accept(logTarget, b); e == nil {
		t.Fatal("oversized line accepted")
	}
	b = batch("retained")
	b.FirstSequence = 99
	b.Records[0].Sequence = 99
	acceptReplay(t, s, b)
	if r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{}); !errors.Is(e, ErrLogHistoryLost) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("truncated replay %v", e)
	}
}

func TestLogsRejectCompletionWithoutReplayBoundary(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := batch()
	b.Complete = true
	b.ReplayComplete = false
	if _, e := s.Accept(logTarget, b); e == nil {
		t.Fatal("completion before drain accepted")
	}
}

func TestLogsTailZeroDoesNotConsumeStreamSlot(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxStreams: 1})
	acceptReplay(t, s, batch())
	zero := 0
	for i := 0; i < 3; i++ {
		r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{Tail: &zero})
		if e != nil {
			t.Fatal(e)
		}
		io.ReadAll(r)
		r.Close()
	}
}
func TestLogsInvalidFirstBatchDoesNotAllocate(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxWorkloads: 1})
	b := batch("bad")
	a, _ := s.Accept(logTarget, b)
	b.Epoch = a.Epoch
	b.Records[0].Sequence = 9
	if _, e := s.Accept(logTarget, b); e == nil {
		t.Fatal("gap accepted")
	}
	if len(s.states) != 0 {
		t.Fatal("invalid batch allocated workload state")
	}
}

func TestLogsRunnerRetentionGapRecoversWithExplicitLoss(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := acceptReplay(t, s, batch("old"))
	b.FirstSequence = 9
	b.Records = []LogRecord{{Sequence: 9, Timestamp: time.Now(), Line: "new"}}
	if _, e := s.Accept(logTarget, b); e != nil {
		t.Fatalf("retained replay could not resume: %v", e)
	}
	if r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{}); !errors.Is(e, ErrLogHistoryLost) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("missing history silently accepted: %v", e)
	}
	one := 1
	r, e := s.OpenLogs(context.Background(), logTarget, core.LogOptions{Tail: &one})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	out, e := io.ReadAll(r)
	if e != nil || string(out) != "new\n" {
		t.Fatalf("retained tail %q %v", out, e)
	}
}

func TestLogsRetentionCoverage(t *testing.T) {
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	one, two, zero := 1, 2, 0
	cases := []struct {
		name     string
		times    []time.Time
		maxBytes int
		first    uint64
		opts     core.LogOptions
		want     string
		lost     bool
	}{
		{name: "equal timestamp straddles eviction", times: []time.Time{stamp, stamp}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp}, lost: true},
		{name: "nonmonotonic lost timestamp is later", times: []time.Time{stamp.Add(time.Second), stamp}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(time.Second)}, lost: true},
		{name: "loss maximum survives subsequent older evictions", times: []time.Time{stamp.Add(5 * time.Second), stamp, stamp.Add(time.Second)}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(2 * time.Second)}, lost: true},
		{name: "cutoff strictly after all known loss", times: []time.Time{stamp, stamp.Add(time.Second)}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(time.Nanosecond)}, want: "r1\n"},
		{name: "strict cutoff proves complete short tail", times: []time.Time{stamp, stamp.Add(time.Second)}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(time.Nanosecond), Tail: &two}, want: "r1\n"},
		{name: "strict cutoff proves empty tail", times: []time.Time{stamp, stamp}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(time.Nanosecond), Tail: &two}},
		{name: "strict cutoff can prove empty result", times: []time.Time{stamp, stamp}, maxBytes: 100, opts: core.LogOptions{SinceTime: stamp.Add(time.Nanosecond)}},
		{name: "unknown prefix cannot prove since coverage", times: []time.Time{stamp, stamp.Add(time.Second)}, first: 9, opts: core.LogOptions{SinceTime: stamp.Add(2 * time.Second)}, lost: true},
		{name: "since plus tail lacks qualifying records", times: []time.Time{stamp.Add(2 * time.Second), stamp, stamp.Add(time.Second)}, maxBytes: 140, opts: core.LogOptions{SinceTime: stamp.Add(time.Second), Tail: &two}, lost: true},
		{name: "since plus tail has enough qualifying records", times: []time.Time{stamp.Add(2 * time.Second), stamp, stamp.Add(time.Second)}, maxBytes: 140, opts: core.LogOptions{SinceTime: stamp.Add(time.Second), Tail: &one}, want: "r2\n"},
		{name: "unknown prefix tail still has qualifying records", times: []time.Time{stamp, stamp.Add(time.Second)}, first: 9, opts: core.LogOptions{SinceTime: stamp.Add(time.Second), Tail: &one}, want: "r1\n"},
		{name: "unknown prefix tail lacks qualifying records", times: []time.Time{stamp, stamp.Add(time.Second)}, first: 9, opts: core.LogOptions{SinceTime: stamp.Add(time.Second), Tail: &two}, lost: true},
		{name: "zero tail requires no history", times: []time.Time{stamp, stamp}, maxBytes: 100, opts: core.LogOptions{Tail: &zero}},
		{name: "zero tail follow requires no history", times: []time.Time{stamp, stamp}, maxBytes: 100, opts: core.LogOptions{Tail: &zero, Follow: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLogStore(LogStoreConfig{MaxBytesPerWorkload: tc.maxBytes})
			b := batch()
			b.Complete = true
			if tc.first == 0 {
				tc.first = 1
			}
			b.FirstSequence = tc.first
			for i, ts := range tc.times {
				b.Records = append(b.Records, LogRecord{Sequence: tc.first + uint64(i), Timestamp: ts, Line: fmt.Sprintf("r%d", i)})
			}
			acceptReplay(t, s, b)
			r, err := s.OpenLogs(context.Background(), logTarget, tc.opts)
			var out []byte
			if err == nil {
				out, err = io.ReadAll(r)
				r.Close()
			}
			if tc.lost {
				if !errors.Is(err, ErrLogHistoryLost) {
					t.Fatalf("logs %q err=%v; want history loss", out, err)
				}
			} else if err != nil || string(out) != tc.want {
				t.Fatalf("logs %q err=%v; want %q", out, err, tc.want)
			}
		})
	}
}

func TestLogsRunnerGapDoesNotProveSinceCoverage(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := acceptReplay(t, s, batch("old"))
	b.FirstSequence = 9
	b.Records = []LogRecord{{Sequence: 9, Timestamp: time.Now(), Line: "retained"}}
	b.Complete = true
	if _, err := s.Accept(logTarget, b); err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenLogs(context.Background(), logTarget, core.LogOptions{SinceTime: b.Records[0].Timestamp.Add(time.Hour)})
	if r != nil {
		r.Close()
	}
	if !errors.Is(err, ErrLogHistoryLost) {
		t.Fatalf("unknown VM gap claimed since coverage: %v", err)
	}
}

func TestLogsFirstProducerSnapshotRechecksCoverage(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxBytesPerWorkload: 140})
	b := acceptReplay(t, s, batch("r0", "r1"))
	opts := core.LogOptions{SinceTime: b.Records[0].Timestamp}
	s.mu.Lock()
	covered := s.states["ns/uid"].covers(opts)
	s.mu.Unlock()
	if !covered {
		t.Fatal("initial OpenLogs coverage should pass")
	}
	// Evict a matching timestamp after the upfront check but before the producer
	// takes its first snapshot; exercise the real producer without scheduler races.
	b.Records = []LogRecord{{Sequence: 3, Timestamp: b.Records[0].Timestamp, Line: "r2"}}
	b.Complete = true
	if _, err := s.Accept(logTarget, b); err != nil {
		t.Fatal(err)
	}
	emitted := 0
	err := s.produceLogs(context.Background(), logTarget, opts, func(logstream.Event) error { emitted++; return nil })
	if !errors.Is(err, ErrLogHistoryLost) || emitted != 0 {
		t.Fatalf("first snapshot emitted %d events with err=%v; want history loss before output", emitted, err)
	}
}

func TestLogsStartedProducerStillRejectsSequenceGaps(t *testing.T) {
	s := NewLogStore(LogStoreConfig{MaxBytesPerWorkload: 140})
	b := acceptReplay(t, s, batch("r0", "r1"))
	boundary := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- s.produceLogs(ctx, logTarget, core.LogOptions{Follow: true}, func(e logstream.Event) error {
			if e.ReplayComplete {
				close(boundary)
				select {
				case <-resume:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
	}()
	select {
	case <-boundary:
	case <-time.After(5 * time.Second):
		t.Fatal("producer never reached replay boundary")
	}
	b.Records = []LogRecord{{Sequence: 3, Timestamp: time.Now(), Line: "r2"}, {Sequence: 4, Timestamp: time.Now(), Line: "r3"}, {Sequence: 5, Timestamp: time.Now(), Line: "r4"}}
	b.Complete = true
	if _, err := s.Accept(logTarget, b); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case err := <-done:
		if !errors.Is(err, ErrLogHistoryLost) {
			t.Fatalf("sequence gap err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sequence gap did not finish producer")
	}
}

func TestLogsRecordCountEvictionTracksCoverage(t *testing.T) {
	s := NewLogStore(LogStoreConfig{})
	b := batch()
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5000; i++ {
		b.Records = append(b.Records, LogRecord{Sequence: uint64(i + 1), Timestamp: stamp, Line: "r"})
	}
	b = acceptReplay(t, s, b)
	b.Records = []LogRecord{{Sequence: 5001, Timestamp: stamp, Line: "r"}}
	b.Complete = true
	if _, err := s.Accept(logTarget, b); err != nil {
		t.Fatal(err)
	}
	r, err := s.OpenLogs(context.Background(), logTarget, core.LogOptions{SinceTime: stamp})
	if r != nil {
		r.Close()
	}
	if !errors.Is(err, ErrLogHistoryLost) {
		t.Fatalf("record-count eviction accepted incomplete range: %v", err)
	}
	n := 5000
	r, err = s.OpenLogs(context.Background(), logTarget, core.LogOptions{SinceTime: stamp, Tail: &n})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil || strings.Count(string(out), "\n") != n {
		t.Fatalf("full retained tail lines=%d err=%v", strings.Count(string(out), "\n"), err)
	}
}

func TestLogsStartedEmptyProducerDetectsRetentionGaps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first uint64
		bytes int
	}{{"VM gap after empty snapshot", 9, 100}, {"all new records evicted", 1, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLogStore(LogStoreConfig{MaxBytesPerWorkload: tc.bytes})
			b := acceptReplay(t, s, batch())
			boundary, resume := make(chan struct{}), make(chan struct{})
			done := make(chan error, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				done <- s.produceLogs(ctx, logTarget, core.LogOptions{Follow: true}, func(e logstream.Event) error {
					if e.ReplayComplete {
						close(boundary)
						select {
						case <-resume:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return nil
				})
			}()
			select {
			case <-boundary:
			case <-time.After(5 * time.Second):
				t.Fatal("empty producer did not start")
			}
			b.FirstSequence = tc.first
			b.Records = []LogRecord{{Sequence: tc.first, Timestamp: time.Now(), Line: "r"}}
			b.Complete = true
			if _, err := s.Accept(logTarget, b); err != nil {
				t.Fatal(err)
			}
			close(resume)
			select {
			case err := <-done:
				if !errors.Is(err, ErrLogHistoryLost) {
					t.Fatalf("empty-start gap err=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("empty-start gap did not finish producer")
			}
		})
	}
}
