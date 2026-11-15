package runpod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestLogsSSEQueriesAndOptions(t *testing.T) {
	n := 2
	since := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pods/pod-test/logs" || r.Header.Get("Authorization") != "Bearer test-key" || r.URL.Query().Get("source") != "container" {
			t.Errorf("request=%s headers=%v", r.URL, r.Header)
		}
		if r.URL.Query().Get("since") != "2026-11-14T00:00:00Z" || r.URL.Query().Has("tail") {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": heartbeat\n\ndata: {\"source\":\"system\",\"ts\":\"2026-11-14T00:00:00Z\",\"line\":\"hidden\"}\n\n")
		for i, line := range []string{"one", "two", "three"} {
			fmt.Fprintf(w, "id: event-%d\ndata: {\"source\":\"container\",\"ts\":\"2026-11-14T00:00:0%dZ\",\"line\":%q}\n\n", i, i, line)
		}
	}))
	defer s.Close()
	c := NewClient(Config{BaseURL: s.URL, APIKey: "test-key"})
	r, err := c.OpenLogs(context.Background(), core.LogTarget{RemoteID: "pod-test"}, core.LogOptions{Tail: &n, SinceTime: since, Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "2026-11-14T00:00:01Z two\n2026-11-14T00:00:02Z three\n" {
		t.Fatalf("logs=%q err=%v", b, err)
	}
}

func TestLogsNoImplicitHundredLineLimit(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") != "1970-01-01T00:00:00Z" {
			t.Errorf("since=%q", r.URL.Query().Get("since"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 120; i++ {
			fmt.Fprintf(w, "data: {\"source\":\"container\",\"ts\":\"2026-11-14T00:00:00Z\",\"line\":\"line-%d\"}\n\n", i)
		}
	}))
	defer s.Close()
	r, err := NewClient(Config{BaseURL: s.URL}).OpenLogs(context.Background(), core.LogTarget{RemoteID: "pod-test"}, core.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || strings.Count(string(b), "\n") != 120 {
		t.Fatalf("lines=%d err=%v", strings.Count(string(b), "\n"), err)
	}
}

func TestLogsErrorAndCancellation(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
		defer s.Close()
		_, err := NewClient(Config{BaseURL: s.URL}).OpenLogs(context.Background(), core.LogTarget{RemoteID: "gone"}, core.LogOptions{})
		if !errors.Is(err, core.ErrRemoteNotFound) {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("close", func(t *testing.T) {
		stopped := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(stopped)
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer s.Close()
		c := NewClient(Config{BaseURL: s.URL, HTTPClient: &http.Client{Timeout: time.Millisecond}})
		r, err := c.OpenLogs(context.Background(), core.LogTarget{RemoteID: "pod-test"}, core.LogOptions{Follow: true})
		if err != nil {
			t.Fatal(err)
		}
		r.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("upstream leaked")
		}
	})
	t.Run("bad SSE", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: not-json\n\n")
		}))
		defer s.Close()
		r, err := NewClient(Config{BaseURL: s.URL}).OpenLogs(context.Background(), core.LogTarget{RemoteID: "pod-test"}, core.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("malformed SSE treated as empty logs")
		}
	})
}
