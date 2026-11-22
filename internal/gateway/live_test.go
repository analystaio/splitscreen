package gateway

import (
	"context"
	"sync"
	"testing"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// liveSurface is a streamingSurface that can also keep a turn in one live
// message, recording what that message was sent.
type liveSurface struct {
	streamingSurface

	lmu     sync.Mutex
	opens   []surface.Post
	updates []surface.StreamUpdate
	closes  []surface.StreamUpdate
}

func (f *liveSurface) OpenLive(_ context.Context, p surface.Post) (surface.Stream, error) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	f.opens = append(f.opens, p)
	return &liveStream{srf: f, ref: surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: "live1"}}, nil
}

func (f *liveSurface) state() (opens []surface.Post, updates, closes []surface.StreamUpdate) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	return append(opens, f.opens...), append(updates, f.updates...), append(closes, f.closes...)
}

type liveStream struct {
	srf *liveSurface
	ref surface.Ref
}

func (s *liveStream) Ref() surface.Ref { return s.ref }
func (s *liveStream) Append(_ context.Context, u surface.StreamUpdate) error {
	s.srf.lmu.Lock()
	defer s.srf.lmu.Unlock()
	s.srf.updates = append(s.srf.updates, u)
	return nil
}
func (s *liveStream) Close(_ context.Context, u surface.StreamUpdate) error {
	s.srf.lmu.Lock()
	defer s.srf.lmu.Unlock()
	s.srf.closes = append(s.srf.closes, u)
	return nil
}

func newLiveHarness(t *testing.T) (*harness, *liveSurface) {
	t.Helper()
	h := newHarness(t)
	cfg := *h.gw.cfg.Load()
	cfg.Gateway.Slack.SingleMessage = true
	h.gw.cfg.Store(&cfg)
	srf := &liveSurface{}
	srf.channels = h.srf.channels
	h.gw.surfaces["test"] = srf
	return h, srf
}

// A turn kept in one message has that message before the runner has said
// anything, carrying its own stop control, and no separate control follows.
func TestLiveMessageOpensWithTheTurnAndCarriesStop(t *testing.T) {
	quickStopControl(t)
	h, srf := newLiveHarness(t)
	ws := h.connect(t, "s3cret")
	readFrame[*protocol.HelloAck](t, ws)

	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Addressed: true, Text: "do a long thing",
	})
	msg := readFrame[*protocol.Message](t, ws)

	opens, _, _ := srf.state()
	if len(opens) != 1 || opens[0].StopTurnID != msg.TurnID || opens[0].Thread != "T1" {
		t.Fatalf("message not opened with the turn: %+v", opens)
	}

	// Prose is shown as a draft while its fate is open, and withdrawn once
	// more work turns it into a step.
	send(t, ws, &protocol.TextDelta{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Text: "Checking the disk."})
	eventually(t, "draft shown", func() bool {
		_, updates, _ := srf.state()
		return len(updates) > 0 && updates[len(updates)-1].Draft == "Checking the disk."
	})
	send(t, ws, &protocol.ToolStart{ThreadID: msg.ThreadID, TurnID: msg.TurnID, CallID: "c1", Tool: "Bash", Summary: "df -h"})
	eventually(t, "draft folded into the steps", func() bool {
		_, updates, _ := srf.state()
		last := updates[len(updates)-1]
		return last.Draft == "" && len(last.Steps) > 0
	})

	// Stopping shows on the message itself.
	h.gw.OnStop(context.Background(), surface.StopRequest{
		Surface: "test", TurnID: msg.TurnID, Inline: true, User: surface.User{ID: "U2"},
	})
	if c := readFrame[*protocol.TurnCancel](t, ws); c.TurnID != msg.TurnID {
		t.Fatalf("cancel for %q, want %q", c.TurnID, msg.TurnID)
	}
	eventually(t, "stopping shown", func() bool {
		_, updates, _ := srf.state()
		return updates[len(updates)-1].Stopping != ""
	})

	send(t, ws, &protocol.Done{ThreadID: msg.ThreadID, TurnID: msg.TurnID, Interrupted: true})
	eventually(t, "message closed", func() bool {
		_, _, closes := srf.state()
		return len(closes) == 1 && closes[0].Footer != ""
	})

	if opens, _, _ := srf.state(); len(opens) != 1 {
		t.Fatalf("turn used %d messages, want 1", len(opens))
	}
	if controls, _, _ := srf.stopState(); len(controls) != 0 {
		t.Fatalf("a separate stop control was posted: %+v", controls)
	}
	if srf.opened != 0 {
		t.Fatalf("a native stream was opened %d times alongside the live message", srf.opened)
	}
}

// A press of a stop button left on a finished turn's message must not replace
// that message with the answer to the press.
func TestSpentInlineStopDoesNotReplaceTheMessage(t *testing.T) {
	h, srf := newLiveHarness(t)
	h.gw.OnStop(context.Background(), surface.StopRequest{
		Surface: "test", TurnID: "turn_gone", Inline: true, User: surface.User{ID: "U1"},
		Ref: surface.Ref{Channel: "C1", Thread: "T1", ID: "live1"},
	})
	if _, _, resolved := srf.stopState(); len(resolved) != 0 {
		t.Fatalf("the turn's message was replaced: %v", resolved)
	}
}
