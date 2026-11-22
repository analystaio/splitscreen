// Package otlp sends log records to an OpenTelemetry collector over OTLP/HTTP,
// encoded as JSON.
//
// It is deliberately small: one signal (logs), one encoding, no SDK. The
// gateway emits a handful of records per minute, and keeping the OpenTelemetry
// SDK out of the build keeps the dependency graph the size of the problem.
//
// Export never blocks the caller and never fails a turn. Records queue in
// memory, go out in batches, and are dropped — counted, and logged once per
// batch — when the queue is full or the collector keeps refusing them. Records
// are a copy of what the audit log holds, so losing some is a gap in a
// dashboard, not in the record.
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Severity numbers, from the OTLP data model.
const (
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17
)

// Record is one log record. Attribute values may be string, bool, int, int64,
// or float64; anything else is sent as its fmt %v string.
type Record struct {
	Time     time.Time
	Severity int
	Body     string
	Attrs    []Attr
}

// Attr is one attribute.
type Attr struct {
	Key   string
	Value any
}

// Target says where records go. It is read per batch, so a config reload can
// point the exporter elsewhere, or switch it off, without a restart.
type Target struct {
	Endpoint string
	Headers  map[string]string
}

// Options configure an Exporter.
type Options struct {
	// Target returns the current destination. An empty Endpoint means
	// disabled: queued records are discarded.
	Target func() Target

	// Resource attributes identify the sender (service.name and so on).
	Resource []Attr
	// Scope names the instrumentation scope.
	Scope string

	Logger *slog.Logger
	Client *http.Client

	// QueueSize bounds records waiting to be sent. Default 1000.
	QueueSize int
	// BatchSize is the most records per request. Default 50.
	BatchSize int
	// Interval is how long a partial batch waits. Default 5s.
	Interval time.Duration
	// Attempts per batch before it is dropped. Default 3.
	Attempts int
}

// Exporter queues and sends records. Construct with New, start with Run.
type Exporter struct {
	o     Options
	queue chan Record
	// Dropped counts records lost to a full queue or a failed send.
	dropped atomic.Int64
	sent    atomic.Int64
}

// New builds an exporter. It sends nothing until Run is called.
func New(o Options) *Exporter {
	if o.Target == nil {
		o.Target = func() Target { return Target{} }
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 1000
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 50
	}
	if o.Interval <= 0 {
		o.Interval = 5 * time.Second
	}
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	return &Exporter{o: o, queue: make(chan Record, o.QueueSize)}
}

// Emit queues a record. It never blocks: with the queue full the record is
// dropped and counted. Records emitted while the exporter is disabled are
// discarded at once.
func (e *Exporter) Emit(r Record) {
	if e == nil || e.o.Target().Endpoint == "" {
		return
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	select {
	case e.queue <- r:
	default:
		e.dropped.Add(1)
	}
}

// Stats reports records sent and dropped since start.
func (e *Exporter) Stats() (sent, dropped int64) {
	return e.sent.Load(), e.dropped.Load()
}

// Run sends queued records until ctx ends, then makes one last attempt to send
// what is left, bounded by a short timeout so shutdown is never held hostage by
// an unreachable collector.
func (e *Exporter) Run(ctx context.Context) {
	tick := time.NewTicker(e.o.Interval)
	defer tick.Stop()
	var batch []Record
	for {
		select {
		case r := <-e.queue:
			batch = append(batch, r)
			if len(batch) >= e.o.BatchSize {
				e.send(ctx, batch)
				batch = nil
			}
		case <-tick.C:
			if len(batch) > 0 {
				e.send(ctx, batch)
				batch = nil
			}
		case <-ctx.Done():
		drain:
			for {
				select {
				case r := <-e.queue:
					batch = append(batch, r)
				default:
					break drain
				}
			}
			fctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for len(batch) > 0 {
				n := min(len(batch), e.o.BatchSize)
				e.sendAttempts(fctx, batch[:n], 1)
				batch = batch[n:]
			}
			return
		}
	}
}

func (e *Exporter) send(ctx context.Context, batch []Record) {
	e.sendAttempts(ctx, batch, e.o.Attempts)
}

func (e *Exporter) sendAttempts(ctx context.Context, batch []Record, attempts int) {
	t := e.o.Target()
	if t.Endpoint == "" {
		return // switched off while these waited
	}
	body, err := json.Marshal(e.encode(batch))
	if err != nil {
		e.drop(batch, "encode", err)
		return
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				e.drop(batch, "send", ctx.Err())
				return
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		retry, err := e.post(ctx, t, body)
		if err == nil {
			e.sent.Add(int64(len(batch)))
			return
		}
		lastErr = err
		if !retry {
			break
		}
	}
	e.drop(batch, "send", lastErr)
}

func (e *Exporter) drop(batch []Record, stage string, err error) {
	e.dropped.Add(int64(len(batch)))
	e.o.Logger.Warn("otlp export dropped records", "stage", stage, "records", len(batch), "err", err)
}

// post sends one request. retry reports whether the failure is worth another
// attempt: network errors, 429 and 5xx are; other statuses mean the collector
// will never accept this payload.
func (e *Exporter) post(ctx context.Context, t Target, body []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.o.Client.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// A 2xx can still report partial rejection; that is the collector's
		// verdict on the records, and resending would not change it.
		return false, nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("collector answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	default:
		return false, fmt.Errorf("collector answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
}

// ---------------------------------------------------------------------------
// OTLP JSON encoding (opentelemetry-proto, logs/v1). 64-bit integers and
// timestamps are strings, as the JSON mapping of protobuf requires.
// ---------------------------------------------------------------------------

type exportRequest struct {
	ResourceLogs []resourceLogs `json:"resourceLogs"`
}

type resourceLogs struct {
	Resource  resource    `json:"resource"`
	ScopeLogs []scopeLogs `json:"scopeLogs"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeLogs struct {
	Scope      scope       `json:"scope"`
	LogRecords []logRecord `json:"logRecords"`
}

type scope struct {
	Name string `json:"name"`
}

type logRecord struct {
	TimeUnixNano         string     `json:"timeUnixNano"`
	ObservedTimeUnixNano string     `json:"observedTimeUnixNano"`
	SeverityNumber       int        `json:"severityNumber,omitempty"`
	SeverityText         string     `json:"severityText,omitempty"`
	Body                 anyValue   `json:"body"`
	Attributes           []keyValue `json:"attributes,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

func (e *Exporter) encode(batch []Record) exportRequest {
	now := strconv.FormatInt(time.Now().UnixNano(), 10)
	recs := make([]logRecord, 0, len(batch))
	for _, r := range batch {
		recs = append(recs, logRecord{
			TimeUnixNano:         strconv.FormatInt(r.Time.UnixNano(), 10),
			ObservedTimeUnixNano: now,
			SeverityNumber:       r.Severity,
			SeverityText:         severityText(r.Severity),
			Body:                 value(r.Body),
			Attributes:           attrs(r.Attrs),
		})
	}
	return exportRequest{ResourceLogs: []resourceLogs{{
		Resource:  resource{Attributes: attrs(e.o.Resource)},
		ScopeLogs: []scopeLogs{{Scope: scope{Name: e.o.Scope}, LogRecords: recs}},
	}}}
}

func severityText(n int) string {
	switch {
	case n == 0:
		return ""
	case n < SeverityWarn:
		return "INFO"
	case n < SeverityError:
		return "WARN"
	default:
		return "ERROR"
	}
}

func attrs(in []Attr) []keyValue {
	out := make([]keyValue, 0, len(in))
	for _, a := range in {
		out = append(out, keyValue{Key: a.Key, Value: value(a.Value)})
	}
	return out
}

func value(v any) anyValue {
	switch x := v.(type) {
	case string:
		return anyValue{StringValue: &x}
	case bool:
		return anyValue{BoolValue: &x}
	case int:
		s := strconv.Itoa(x)
		return anyValue{IntValue: &s}
	case int64:
		s := strconv.FormatInt(x, 10)
		return anyValue{IntValue: &s}
	case float64:
		return anyValue{DoubleValue: &x}
	default:
		s := fmt.Sprint(x)
		return anyValue{StringValue: &s}
	}
}
