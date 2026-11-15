package runner

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
)

const MaxLogBatchBytes = 2 << 20

var ErrLogHistoryLost = errors.New("retained log history has been truncated")

type LogRecord struct {
	Sequence  uint64    `json:"sequence"`
	Timestamp time.Time `json:"timestamp"`
	Line      string    `json:"line"`
}
type LogBatch struct {
	WorkloadID     string      `json:"workloadID"`
	Generation     string      `json:"generation"`
	Epoch          string      `json:"epoch"`
	FirstSequence  uint64      `json:"firstSequence"`
	Records        []LogRecord `json:"records"`
	ReplayComplete bool        `json:"replayComplete"`
	Complete       bool        `json:"complete"`
	Error          string      `json:"error,omitempty"`
}
type LogAck struct {
	Epoch          string `json:"epoch"`
	NextSequence   uint64 `json:"nextSequence"`
	ReplayRequired bool   `json:"replayRequired"`
}
type LogStoreConfig struct {
	MaxBytesPerWorkload, MaxWorkloads, MaxStreams int
	ReplayTimeout                                 time.Duration
}
type logState struct {
	target                 core.LogTarget
	generation             string
	records                []LogRecord
	next                   uint64
	size                   int
	replay, complete, lost bool
	unknownPrefixLoss      bool      // VM retention removed records whose timestamps were never uploaded.
	lostThrough            time.Time // Maximum timestamp evicted by this controller.
	failure                string
	updated                time.Time
	changed                chan struct{}
}
type LogStore struct {
	mu      sync.Mutex
	config  LogStoreConfig
	epoch   string
	states  map[string]*logState
	streams int
}

func NewLogStore(c LogStoreConfig) *LogStore {
	if c.MaxBytesPerWorkload <= 0 {
		c.MaxBytesPerWorkload = 4 << 20
	}
	if c.MaxWorkloads <= 0 {
		c.MaxWorkloads = 128
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 64
	}
	if c.ReplayTimeout <= 0 {
		c.ReplayTimeout = 45 * time.Second
	}
	token := make([]byte, 16)
	if _, e := rand.Read(token); e != nil {
		panic(e)
	}
	return &LogStore{config: c, epoch: hex.EncodeToString(token), states: map[string]*logState{}}
}
func logID(t core.LogTarget) string { return t.Namespace + "/" + t.WorkloadUID }

// Accept must only be called after authenticating the body and resolving a live
// Lambda binding. It never allocates state for a reader or an unknown workload.
func (s *LogStore) Accept(t core.LogTarget, b LogBatch) (LogAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := LogAck{Epoch: s.epoch, NextSequence: 1}
	if t.Namespace == "" || t.WorkloadUID == "" || t.RemoteID == "" || b.WorkloadID != logID(t) || b.Generation == "" {
		return a, fmt.Errorf("invalid log workload identity")
	}
	if len(b.Records) > 5000 || len(b.Error) > 1024 || b.FirstSequence == 0 || (b.Complete && (!b.ReplayComplete || b.Error != "")) {
		return a, fmt.Errorf("malformed log batch")
	}
	total := 0
	for i, r := range b.Records {
		total += len(r.Line) + 64
		if len(r.Line) > logstream.MaxLineBytes || r.Timestamp.IsZero() || r.Sequence == 0 || r.Sequence == ^uint64(0) || (i > 0 && r.Sequence != b.Records[i-1].Sequence+1) {
			return a, fmt.Errorf("malformed or out-of-order log record")
		}
	}
	if total > MaxLogBatchBytes {
		return a, fmt.Errorf("log batch exceeds byte limit")
	}
	st := s.states[b.WorkloadID]
	if st != nil && (st.target.RemoteID != t.RemoteID || st.generation != b.Generation) {
		return a, fmt.Errorf("stale log generation or foreign remote identity")
	}
	if b.Epoch != s.epoch {
		a.ReplayRequired = true
		return a, nil
	}
	if st == nil {
		if len(s.states) >= s.config.MaxWorkloads {
			return a, fmt.Errorf("log workload capacity exceeded")
		}
		st = &logState{target: t, generation: b.Generation, next: b.FirstSequence, lost: b.FirstSequence > 1, unknownPrefixLoss: b.FirstSequence > 1, changed: make(chan struct{})}
	}
	a.NextSequence = st.next
	// Validate the entire batch before mutation. Retries below next are duplicates.
	next := st.next
	retentionGap := b.FirstSequence > next
	if retentionGap {
		next = b.FirstSequence
	}
	for _, r := range b.Records {
		if r.Sequence < next {
			continue
		}
		if r.Sequence != next {
			return a, fmt.Errorf("log sequence gap: want %d, got %d", next, r.Sequence)
		}
		if st.complete {
			return a, fmt.Errorf("records after log completion")
		}
		next++
	}
	if retentionGap {
		st.next = b.FirstSequence
		st.records = nil
		st.size = 0
		st.lost = true
		st.unknownPrefixLoss = true
	}
	s.states[b.WorkloadID] = st
	for _, r := range b.Records {
		if r.Sequence < st.next {
			continue
		}
		st.records = append(st.records, r)
		st.size += len(r.Line) + 64
		st.next++
		for st.size > s.config.MaxBytesPerWorkload || len(st.records) > 5000 {
			if st.records[0].Timestamp.After(st.lostThrough) {
				st.lostThrough = st.records[0].Timestamp
			}
			st.size -= len(st.records[0].Line) + 64
			st.records[0] = LogRecord{}
			st.records = st.records[1:]
			st.lost = true
		}
	}
	st.replay = st.replay || b.ReplayComplete
	st.complete = st.complete || b.Complete
	st.failure = b.Error
	st.updated = time.Now()
	close(st.changed)
	st.changed = make(chan struct{})
	a.NextSequence = st.next
	return a, nil
}

// Forget releases retained state when the binding is deleted.
func (s *LogStore) Forget(t core.LogTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.states[logID(t)]; st != nil && st.target.RemoteID == t.RemoteID {
		delete(s.states, logID(t))
		close(st.changed)
	}
}
func (s *LogStore) OpenLogs(ctx context.Context, t core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	st := s.states[logID(t)]
	if st == nil || st.target.RemoteID != t.RemoteID {
		s.mu.Unlock()
		return nil, fmt.Errorf("runner logs unavailable: awaiting retained replay")
	}
	if !st.covers(o) {
		s.mu.Unlock()
		return nil, ErrLogHistoryLost
	}
	if o.Tail != nil && *o.Tail == 0 && !o.Follow {
		s.mu.Unlock()
		return logstream.Open(ctx, o, nil, logstream.Timing{NativeReplay: true})
	}
	if !st.complete && time.Since(st.updated) >= s.config.ReplayTimeout {
		s.mu.Unlock()
		return nil, fmt.Errorf("runner log transport timed out")
	}
	if s.streams >= s.config.MaxStreams {
		s.mu.Unlock()
		return nil, fmt.Errorf("log stream capacity exceeded")
	}
	s.streams++
	s.mu.Unlock()
	r, err := logstream.Open(ctx, o, func(ctx context.Context, emit func(logstream.Event) error) error {
		defer func() { s.mu.Lock(); s.streams--; s.mu.Unlock() }()
		return s.produceLogs(ctx, t, o, emit)
	}, logstream.Timing{NativeReplay: true})
	if err != nil {
		s.mu.Lock()
		s.streams--
		s.mu.Unlock()
	}
	return r, err
}

// covers proves that retention has not removed records required by these options.
// Since filters are inclusive; timestamps can be shared and nonmonotonic.
func (st *logState) covers(o core.LogOptions) bool {
	if !st.lost {
		return true
	}
	if !st.unknownPrefixLoss && !o.SinceTime.IsZero() && o.SinceTime.After(st.lostThrough) {
		return true
	}
	if o.Tail != nil {
		if *o.Tail == 0 {
			return true
		}
		matching := 0
		for _, r := range st.records {
			if o.SinceTime.IsZero() || !r.Timestamp.Before(o.SinceTime) {
				matching++
				if matching >= *o.Tail {
					return true
				}
			}
		}
		return false
	}
	return false
}

func (s *LogStore) produceLogs(ctx context.Context, t core.LogTarget, o core.LogOptions, emit func(logstream.Event) error) error {
	next := uint64(0)
	firstSnapshot := true
	boundary := false
	for {
		s.mu.Lock()
		st := s.states[logID(t)]
		if st == nil || st.target.RemoteID != t.RemoteID {
			s.mu.Unlock()
			return fmt.Errorf("log workload deleted")
		}
		if firstSnapshot && !st.covers(o) {
			s.mu.Unlock()
			return ErrLogHistoryLost
		}
		firstRetained := st.next
		if len(st.records) > 0 {
			firstRetained = st.records[0].Sequence
		}
		if firstSnapshot {
			next = firstRetained
		}
		firstSnapshot = false
		if next < firstRetained {
			s.mu.Unlock()
			return ErrLogHistoryLost
		}
		records := append([]LogRecord(nil), st.records...)
		replay, complete, failure, updated, ch := st.replay, st.complete, st.failure, st.updated, st.changed
		s.mu.Unlock()
		if !complete && time.Since(updated) >= s.config.ReplayTimeout {
			return fmt.Errorf("runner log transport timed out")
		}
		for _, v := range records {
			if v.Sequence < next {
				continue
			}
			if e := emit(logstream.Event{Timestamp: v.Timestamp, Line: v.Line}); e != nil {
				return e
			}
			next = v.Sequence + 1
		}
		if failure != "" {
			return fmt.Errorf("runner capture failed: %s", failure)
		}
		if replay && !boundary {
			if e := emit(logstream.Event{ReplayComplete: true}); e != nil {
				return e
			}
			boundary = true
		}
		if complete {
			return nil
		}
		wait := time.Until(updated.Add(s.config.ReplayTimeout))
		if wait <= 0 {
			return fmt.Errorf("runner log transport timed out")
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-ch:
			timer.Stop()
		case <-timer.C:
			return fmt.Errorf("runner log transport timed out")
		}
	}
}

// DeriveLogKey returns a hex key scoped to a workload, never the callback root.
func DeriveLogKey(secret, workloadID string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("neocloud-runner-logs-v1\x00" + workloadID))
	return hex.EncodeToString(m.Sum(nil))
}
func SignLogBatch(secret, workloadID, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(DeriveLogKey(secret, workloadID)))
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return "v1=" + hex.EncodeToString(m.Sum(nil))
}
func VerifyLogBatch(secret, workloadID, timestamp, signature string, body []byte, now time.Time) error {
	if secret == "" {
		return fmt.Errorf("runner log authentication is not configured")
	}
	ts, e := strconv.ParseInt(timestamp, 10, 64)
	if e != nil {
		return fmt.Errorf("invalid log timestamp")
	}
	delta := now.Sub(time.Unix(ts, 0))
	if delta > 5*time.Minute || delta < -5*time.Minute {
		return fmt.Errorf("expired log signature")
	}
	if !hmac.Equal([]byte(signature), []byte(SignLogBatch(secret, workloadID, timestamp, body))) {
		return fmt.Errorf("invalid log signature")
	}
	return nil
}
