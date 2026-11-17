package runpod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
)

var _ core.ProviderLogReader = (*Client)(nil)

// OpenLogs reads container events only. One-shot reads use logstream's bounded
// replay approximation because RunPod does not expose a replay-complete marker.
func (c *Client) OpenLogs(parent context.Context, target core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if target.RemoteID == "" {
		return nil, fmt.Errorf("missing remote resource ID")
	}
	if o.Tail != nil && *o.Tail == 0 && !o.Follow {
		return io.NopCloser(strings.NewReader("")), nil
	}
	if o.Tail != nil && *o.Tail == 0 && !o.SinceTime.IsZero() {
		return nil, fmt.Errorf("RunPod does not support tail=0 with since")
	}
	ctx, cancel := context.WithCancel(parent)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/pods/"+url.PathEscape(target.RemoteID)+"/logs", nil)
	if err != nil {
		cancel()
		return nil, err
	}
	q := req.URL.Query()
	q.Set("source", "container")
	switch {
	case !o.SinceTime.IsZero():
		q.Set("since", o.SinceTime.UTC().Format(time.RFC3339Nano))
	case o.Tail != nil:
		q.Set("tail", strconv.Itoa(*o.Tail))
	default:
		q.Set("since", "1970-01-01T00:00:00Z")
	}
	req.URL.RawQuery = q.Encode()
	c.setHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	client := *c.httpClient
	client.Timeout = 0
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if base, ok := transport.(*http.Transport); ok {
		cloned := base.Clone()
		cloned.DisableKeepAlives = true
		if cloned.ResponseHeaderTimeout == 0 {
			cloned.ResponseHeaderTimeout = 30 * time.Second
		}
		client.Transport = cloned
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, providerTransportError(req, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		cancel()
		return nil, providerHTTPError(req, resp.StatusCode, body)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("RunPod log response is not an SSE stream")
	}
	out, err := logstream.Open(ctx, o, func(streamCtx context.Context, emit func(logstream.Event) error) error {
		done := make(chan struct{})
		defer close(done)
		defer resp.Body.Close()
		go func() {
			select {
			case <-streamCtx.Done():
				resp.Body.Close()
			case <-done:
			}
		}()
		return decodeLogEvents(resp.Body, emit)
	}, logstream.Timing{SnapshotIdle: c.logSnapshotIdle, SnapshotMax: c.logSnapshotMax})
	if err != nil {
		resp.Body.Close()
		cancel()
		return nil, err
	}
	return &runpodLogReader{ReadCloser: out, cancel: cancel}, nil
}

type runpodLogReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *runpodLogReader) Close() error { r.cancel(); return r.ReadCloser.Close() }

func decodeLogEvents(src io.Reader, emit func(logstream.Event) error) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), logstream.MaxLineBytes+1)
	var data strings.Builder
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		var event struct {
			Source    string `json:"source"`
			Line      string `json:"line"`
			Timestamp string `json:"ts"`
		}
		raw := data.String()
		data.Reset()
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return fmt.Errorf("invalid RunPod log event JSON")
		}
		if event.Source != "container" {
			return nil
		}
		ts, err := time.Parse(time.RFC3339Nano, event.Timestamp)
		if err != nil {
			return fmt.Errorf("invalid RunPod log event timestamp")
		}
		return emit(logstream.Event{Timestamp: ts, Line: event.Line})
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			part := strings.TrimPrefix(line, "data:")
			part = strings.TrimPrefix(part, " ")
			if data.Len()+len(part)+1 > logstream.MaxLineBytes {
				return fmt.Errorf("RunPod log frame exceeds size limit")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(part)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read RunPod log stream: %w", err)
	}
	return dispatch()
}
