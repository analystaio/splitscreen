package gateway

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/analystaio/splitscreen/internal/otlp"
	"github.com/analystaio/splitscreen/internal/store"
)

// Turn records: one OTLP log record per finished turn, for tools that cannot
// read the audit log (config.Telemetry). The attribute names below are an
// interface — dashboards and queries are written against them — so rename
// nothing without a reason worth breaking those for.

// newTelemetry builds the exporter. Its destination is read from the live
// config on every batch, so a reload switches it.
func (g *Gateway) newTelemetry(version string) *otlp.Exporter {
	if version == "" {
		version = "dev"
	}
	return otlp.New(otlp.Options{
		Target: g.telemetryTarget,
		Resource: []otlp.Attr{
			{Key: "service.name", Value: "splitscreen-gateway"},
			{Key: "service.version", Value: version},
		},
		Scope:  "github.com/analystaio/splitscreen/gateway",
		Logger: g.log,
	})
}

func (g *Gateway) telemetryTarget() otlp.Target {
	c := g.cfg.Load()
	if c == nil || !c.Gateway.Telemetry.Enabled() {
		return otlp.Target{}
	}
	t := c.Gateway.Telemetry
	out := otlp.Target{Endpoint: t.LogsEndpoint}
	if len(t.Headers) > 0 {
		out.Headers = make(map[string]string, len(t.Headers))
		for h, name := range t.Headers {
			sec, err := g.secrets.Get(name)
			if err != nil {
				// Sent without it: the collector's refusal is logged by the
				// exporter, which says more than a silent skip would.
				g.log.Warn("telemetry header secret unavailable", "header", h, "secret", name, "err", err)
				continue
			}
			out.Headers[h] = sec.Value
		}
	}
	return out
}

// turnEnded is the one place a finished turn is reported, whatever ended it.
func (g *Gateway) turnEnded(ctx context.Context, turn *turnContext, status, errMsg string) {
	g.scheduleTurnEnded(ctx, turn, status, errMsg)
	g.exportTurn(turn, status, errMsg)
}

func (g *Gateway) exportTurn(turn *turnContext, status, errMsg string) {
	if turn == nil || g.telemetry == nil {
		return
	}
	c := g.cfg.Load()
	if c == nil || !c.Gateway.Telemetry.Enabled() {
		return
	}
	g.telemetry.Emit(turnRecord(turn, status, errMsg, c.Gateway.Telemetry.IncludeText, c.Gateway.Telemetry.MaxTextBytes, time.Now()))
}

func turnRecord(turn *turnContext, status, errMsg string, includeText bool, maxText int, now time.Time) otlp.Record {
	rec := turn.record()
	duration := now.Sub(turn.StartedAt)

	a := []otlp.Attr{
		{Key: "splitscreen.turn.id", Value: turn.TurnID},
		{Key: "splitscreen.turn.status", Value: status},
		{Key: "splitscreen.turn.started_at", Value: turn.StartedAt.UTC().Format(time.RFC3339Nano)},
		{Key: "splitscreen.turn.duration_ms", Value: duration.Milliseconds()},
		{Key: "splitscreen.turn.tool_calls", Value: turn.toolCalls.Load()},
		{Key: "splitscreen.thread.id", Value: turn.ThreadID},
		{Key: "splitscreen.surface", Value: turn.Surface},
		{Key: "splitscreen.channel.id", Value: turn.Channel},
		{Key: "splitscreen.channel.private", Value: turn.Private},
		{Key: "splitscreen.channel.dm", Value: turn.IsDM},
		{Key: "splitscreen.thread", Value: turn.Thread},
		{Key: "splitscreen.runner", Value: turn.Runner},
		{Key: "splitscreen.user.id", Value: turn.User.ID},
	}
	opt := func(key, v string) {
		if v != "" {
			a = append(a, otlp.Attr{Key: key, Value: v})
		}
	}
	opt("splitscreen.channel.name", turn.ChannelName)
	opt("splitscreen.user.name", turn.User.Display)
	opt("splitscreen.turn.error", errMsg)
	opt("splitscreen.schedule.id", turn.ScheduleID)
	opt("splitscreen.session.id", rec.sessionID)

	if u := rec.usage; u != nil {
		a = append(a, otlp.Attr{Key: "splitscreen.usage.known", Value: u.Known})
		opt("gen_ai.request.model", u.Model)
		if u.Known {
			a = append(a,
				otlp.Attr{Key: "gen_ai.usage.input_tokens", Value: u.InputTokens},
				otlp.Attr{Key: "gen_ai.usage.output_tokens", Value: u.OutputTokens},
				otlp.Attr{Key: "splitscreen.usage.cache_read_tokens", Value: u.CacheReadTokens},
				otlp.Attr{Key: "splitscreen.usage.cache_write_tokens", Value: u.CacheWriteTokens},
			)
		}
		if u.ComputedUSD != nil {
			a = append(a, otlp.Attr{Key: "splitscreen.cost.computed_usd", Value: *u.ComputedUSD})
		}
		if u.ReportedUSD != nil {
			a = append(a, otlp.Attr{Key: "splitscreen.cost.reported_usd", Value: *u.ReportedUSD})
		}
	}

	if includeText {
		for _, t := range []struct{ key, text string }{
			{"splitscreen.prompt", turn.Prompt},
			{"splitscreen.answer", rec.answer},
		} {
			s, cut := truncateUTF8(t.text, maxText)
			a = append(a, otlp.Attr{Key: t.key, Value: s})
			if cut {
				a = append(a, otlp.Attr{Key: t.key + ".truncated", Value: true})
			}
		}
	}

	sev := otlp.SeverityInfo
	if status == store.TurnError {
		sev = otlp.SeverityError
	}
	where := turn.ChannelName
	switch {
	case turn.IsDM:
		where = "a DM"
	case where == "":
		where = turn.Channel
	default:
		where = "#" + where
	}
	who := turn.User.Display
	if who == "" {
		who = turn.User.ID
	}
	body := fmt.Sprintf("turn %s on %s in %s for %s (%d tool calls, %s)",
		status, turn.Runner, where, who, turn.toolCalls.Load(), duration.Round(time.Second))

	return otlp.Record{Time: now, Severity: sev, Body: body, Attrs: a}
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune. n <= 0
// means no limit.
func truncateUTF8(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n-- // s[n] continues a rune that starts before the cut
	}
	return s[:n], true
}

// turnRecordState is what a turn learns while it runs that its record needs.
// Guarded by turnContext.recMu: the stream, the usage frame and Done arrive on
// different goroutines.
type turnRecordState struct {
	answer    string
	usage     *store.Usage
	sessionID string
}

func (t *turnContext) record() turnRecordState {
	t.recMu.Lock()
	defer t.recMu.Unlock()
	return t.rec
}

func (t *turnContext) setAnswer(s string) {
	t.recMu.Lock()
	t.rec.answer = s
	t.recMu.Unlock()
}

func (t *turnContext) setUsage(u store.Usage) {
	t.recMu.Lock()
	t.rec.usage = &u
	t.recMu.Unlock()
}

func (t *turnContext) setSessionID(id string) {
	t.recMu.Lock()
	t.rec.sessionID = id
	t.recMu.Unlock()
}
