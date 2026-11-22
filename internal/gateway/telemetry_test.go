package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/analystaio/splitscreen/config"
	"github.com/analystaio/splitscreen/internal/otlp"
	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// fakeCollector keeps every log record it receives, attributes flattened to
// key -> JSON value.
type fakeCollector struct {
	mu   sync.Mutex
	recs []map[string]any
	keys []string
}

func (c *fakeCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Body       map[string]any `json:"body"`
					Attributes []struct {
						Key   string         `json:"key"`
						Value map[string]any `json:"value"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys = append(c.keys, r.Header.Get("X-Collector-Key"))
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				m := map[string]any{"body": lr.Body["stringValue"]}
				for _, a := range lr.Attributes {
					for _, v := range a.Value {
						m[a.Key] = v
					}
				}
				c.recs = append(c.recs, m)
			}
		}
	}
}

func (c *fakeCollector) records() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.recs...)
}

// withTelemetry points the harness gateway at a collector and starts the
// exporter, which New builds but only Run starts.
func withTelemetry(t *testing.T, h *harness, includeText bool, maxText int) *fakeCollector {
	t.Helper()
	col := &fakeCollector{}
	srv := httptest.NewServer(col)
	t.Cleanup(srv.Close)

	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.Telemetry = config.Telemetry{
		LogsEndpoint: srv.URL + "/v1/logs",
		Headers:      map[string]string{"X-Collector-Key": "collector-key"},
		IncludeText:  includeText,
		MaxTextBytes: maxText,
	}
	t.Setenv("SPLITSCREEN_SECRET_COLLECTOR_KEY", "k-123")
	h.gw.applyConfig(cfg)
	// The production batch interval (seconds) would outlast eventually().
	h.gw.telemetry = otlp.New(otlp.Options{Target: h.gw.telemetryTarget, Logger: testLogger(), Interval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.gw.telemetry.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return col
}

func runTurn(t *testing.T, h *harness, in surface.Inbound, answer string) {
	t.Helper()
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.OnMessage(context.Background(), in)
	msg := readFrame[*protocol.Message](t, ws)
	send(t, ws, &protocol.ToolStart{ThreadID: msg.ThreadID, TurnID: msg.TurnID, CallID: "c1", Tool: "Bash", Summary: "ls"})
	send(t, ws, &protocol.ToolEnd{ThreadID: msg.ThreadID, TurnID: msg.TurnID, CallID: "c1", OK: true})
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: answer})
	send(t, ws, &protocol.Usage{
		ThreadID: msg.ThreadID, TurnID: msg.TurnID, Model: "claude-sonnet-5",
		InputTokens: 100, CacheReadTokens: 900, OutputTokens: 50, Known: true,
	})
	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, SessionID: "sess-1"})
	eventually(t, "turn finished", func() bool {
		_, running := h.gw.turns.Load(msg.TurnID)
		return !running
	})
}

func TestTurnRecordExported(t *testing.T) {
	h := newHarness(t)
	col := withTelemetry(t, h, true, 0)

	runTurn(t, h, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1", ChannelName: "box-alpha", Private: true,
		User: surface.User{ID: "U1", Display: "Ana"}, Text: "what changed?", Addressed: true,
	}, "nothing changed")

	var rec map[string]any
	eventually(t, "record exported", func() bool {
		if r := col.records(); len(r) == 1 {
			rec = r[0]
			return true
		}
		return false
	})

	want := map[string]any{
		"splitscreen.turn.status":     store.TurnDone,
		"splitscreen.channel.id":      "C1",
		"splitscreen.channel.name":    "box-alpha",
		"splitscreen.channel.private": true,
		"splitscreen.thread":          "T1",
		"splitscreen.runner":          "alpha",
		"splitscreen.user.id":         "U1",
		"splitscreen.user.name":       "Ana",
		"splitscreen.session.id":      "sess-1",
		"splitscreen.turn.tool_calls": "1",
		"splitscreen.prompt":          "what changed?",
		"splitscreen.answer":          "nothing changed",
		"gen_ai.request.model":        "claude-sonnet-5",
		"gen_ai.usage.input_tokens":   "100",
		"gen_ai.usage.output_tokens":  "50",
	}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %#v, want %#v", k, rec[k], v)
		}
	}
	if _, ok := rec["splitscreen.cost.computed_usd"]; !ok {
		t.Error("priced turn exported without its cost")
	}
	if body, _ := rec["body"].(string); !strings.Contains(body, "#box-alpha") || !strings.Contains(body, "Ana") {
		t.Errorf("body = %q", body)
	}
	col.mu.Lock()
	key := col.keys[0]
	col.mu.Unlock()
	if key != "k-123" {
		t.Errorf("header = %q, want the resolved secret", key)
	}
}

func TestTurnRecordOmitsTextByDefault(t *testing.T) {
	h := newHarness(t)
	col := withTelemetry(t, h, false, 0)

	runTurn(t, h, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T2",
		User: surface.User{ID: "U1"}, Text: "secret question", Addressed: true,
	}, "secret answer")

	eventually(t, "record exported", func() bool { return len(col.records()) == 1 })
	rec := col.records()[0]
	for k, v := range rec {
		if s, ok := v.(string); ok && strings.Contains(s, "secret") {
			t.Errorf("%s carries message text with include_text off: %q", k, s)
		}
	}
	if rec["splitscreen.channel.private"] != false {
		t.Errorf("private = %#v", rec["splitscreen.channel.private"])
	}
}

func TestTurnRecordTruncatesText(t *testing.T) {
	h := newHarness(t)
	col := withTelemetry(t, h, true, 8)

	runTurn(t, h, surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T3",
		User: surface.User{ID: "U1"}, Text: "héllo wörld", Addressed: true,
	}, "short")

	eventually(t, "record exported", func() bool { return len(col.records()) == 1 })
	rec := col.records()[0]
	if rec["splitscreen.prompt"] != "héllo w" || rec["splitscreen.prompt.truncated"] != true {
		t.Errorf("prompt = %#v truncated=%#v", rec["splitscreen.prompt"], rec["splitscreen.prompt.truncated"])
	}
	if _, cut := rec["splitscreen.answer.truncated"]; cut {
		t.Error("a short answer was marked truncated")
	}
}

func TestTelemetryOffSendsNothing(t *testing.T) {
	h := newHarness(t)
	if h.gw.telemetryTarget().Endpoint != "" {
		t.Fatal("telemetry on without an endpoint")
	}
	// Must not panic or queue with telemetry off.
	h.gw.exportTurn(&turnContext{TurnID: "t", StartedAt: time.Now()}, store.TurnDone, "")
	if sent, dropped := h.gw.telemetry.Stats(); sent != 0 || dropped != 0 {
		t.Fatalf("sent=%d dropped=%d", sent, dropped)
	}
}

func TestTruncateUTF8(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"abc", 0, "abc", false},
		{"abc", 3, "abc", false},
		{"abcd", 3, "abc", true},
		{"aé", 2, "a", true}, // é is two bytes; never split it
		{"日本", 4, "日", true},
	} {
		got, cut := truncateUTF8(c.in, c.n)
		if got != c.want || cut != c.cut {
			t.Errorf("truncateUTF8(%q, %d) = %q %v, want %q %v", c.in, c.n, got, cut, c.want, c.cut)
		}
	}
}
