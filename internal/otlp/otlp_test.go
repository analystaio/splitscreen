package otlp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type collector struct {
	mu      sync.Mutex
	reqs    []exportRequest
	headers []http.Header
	status  atomic.Int32
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s := c.status.Load(); s != 0 {
		w.WriteHeader(int(s))
		return
	}
	var req exportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.headers = append(c.headers, r.Header.Clone())
	c.mu.Unlock()
}

func (c *collector) records() []logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []logRecord
	for _, r := range c.reqs {
		for _, rl := range r.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				out = append(out, sl.LogRecords...)
			}
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExportEncodesRecords(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()

	e := New(Options{
		Target:   func() Target { return Target{Endpoint: srv.URL, Headers: map[string]string{"X-Key": "k"}} },
		Resource: []Attr{{"service.name", "splitscreen-gateway"}},
		Scope:    "test",
		Logger:   quiet(),
		Interval: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	at := time.Unix(1700000000, 5)
	e.Emit(Record{Time: at, Severity: SeverityInfo, Body: "hello", Attrs: []Attr{
		{"s", "x"}, {"b", true}, {"i", 7}, {"i64", int64(1) << 40}, {"f", 1.5},
	}})
	waitFor(t, func() bool { return len(c.records()) == 1 })

	r := c.records()[0]
	if r.TimeUnixNano != "1700000000000000005" || r.SeverityText != "INFO" || *r.Body.StringValue != "hello" {
		t.Fatalf("record = %+v", r)
	}
	got := map[string]anyValue{}
	for _, kv := range r.Attributes {
		got[kv.Key] = kv.Value
	}
	if *got["s"].StringValue != "x" || !*got["b"].BoolValue || *got["i"].IntValue != "7" ||
		*got["i64"].IntValue != "1099511627776" || *got["f"].DoubleValue != 1.5 {
		t.Fatalf("attrs = %+v", got)
	}
	c.mu.Lock()
	h := c.headers[0]
	res := c.reqs[0].ResourceLogs[0].Resource.Attributes[0]
	c.mu.Unlock()
	if h.Get("X-Key") != "k" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if res.Key != "service.name" || *res.Value.StringValue != "splitscreen-gateway" {
		t.Fatalf("resource = %+v", res)
	}
}

func TestDisabledDiscards(t *testing.T) {
	e := New(Options{Logger: quiet()})
	e.Emit(Record{Body: "x"})
	if len(e.queue) != 0 {
		t.Fatal("disabled exporter queued a record")
	}
	var nilExporter *Exporter
	nilExporter.Emit(Record{}) // must not panic
}

func TestFullQueueDrops(t *testing.T) {
	e := New(Options{Target: func() Target { return Target{Endpoint: "http://127.0.0.1:1"} }, QueueSize: 2, Logger: quiet()})
	for range 5 {
		e.Emit(Record{Body: "x"})
	}
	if _, dropped := e.Stats(); dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
}

func TestRejectedBatchIsDroppedNotRetried(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	e := New(Options{Target: func() Target { return Target{Endpoint: srv.URL} }, Logger: quiet(), Interval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	e.Emit(Record{Body: "x"})
	waitFor(t, func() bool { _, d := e.Stats(); return d == 1 })
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 (4xx is not retried)", hits.Load())
	}
}

func TestShutdownFlushes(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	e := New(Options{Target: func() Target { return Target{Endpoint: srv.URL} }, Logger: quiet(), Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	e.Emit(Record{Body: "a"})
	e.Emit(Record{Body: "b"})
	cancel()
	<-done
	if n := len(c.records()); n != 2 {
		t.Fatalf("flushed %d records, want 2", n)
	}
}
