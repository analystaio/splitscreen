package store

import (
	"errors"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestThreadBindingIsIdempotent(t *testing.T) {
	s := open(t)

	th, created, err := s.BindThread("k1", "slack", "C1", "alpha")
	if err != nil || !created {
		t.Fatalf("first bind: %v created=%v", err, created)
	}
	if th.Runner != "alpha" {
		t.Fatalf("runner = %q", th.Runner)
	}

	// A second bind must return the existing binding, not repoint it: the
	// session lives on the original runner's disk.
	again, created, err := s.BindThread("k1", "slack", "C1", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("second bind reported a creation")
	}
	if again.Runner != "alpha" {
		t.Fatalf("binding moved to %q", again.Runner)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := open(t)
	if _, _, err := s.BindThread("k1", "slack", "C1", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThreadSession("k1", "sess-1"); err != nil {
		t.Fatal(err)
	}
	th, _ := s.Thread("k1")
	if th.SessionID != "sess-1" {
		t.Fatalf("session = %q", th.SessionID)
	}

	// !new drops the resume point so the next message starts over.
	if err := s.ClearSession("k1"); err != nil {
		t.Fatal(err)
	}
	th, _ = s.Thread("k1")
	if th.SessionID != "" {
		t.Fatalf("session survived a clear: %q", th.SessionID)
	}

	// Rebinding moves the runner and abandons the session with it.
	if err := s.SetThreadSession("k1", "sess-2"); err != nil {
		t.Fatal(err)
	}
	if err := s.RebindThread("k1", "beta"); err != nil {
		t.Fatal(err)
	}
	th, _ = s.Thread("k1")
	if th.Runner != "beta" || th.SessionID != "" {
		t.Fatalf("after rebind: runner=%q session=%q", th.Runner, th.SessionID)
	}
}

func TestThreadNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.Thread("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStaleMarking(t *testing.T) {
	s := open(t)
	if _, _, err := s.BindThread("k1", "slack", "C1", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThreadBundleVersion("k1", 1); err != nil {
		t.Fatal(err)
	}

	n, err := s.MarkRunnerThreadsStale("alpha", 2)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("marked %d threads, want 1", n)
	}
	th, _ := s.Thread("k1")
	if !th.Stale {
		t.Error("thread should be stale after a bundle change")
	}

	// Marking twice must not double-count: the announcement happens once.
	n, _ = s.MarkRunnerThreadsStale("alpha", 2)
	if n != 0 {
		t.Errorf("re-marking affected %d rows, want 0", n)
	}
}

func TestUsageKeepsCountersSeparate(t *testing.T) {
	s := open(t)
	if _, _, err := s.BindThread("k1", "slack", "C1", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := s.StartTurn(Turn{ID: "t1", ThreadID: "k1", Channel: "C1", Runner: "alpha", SurfaceUser: "U1"}); err != nil {
		t.Fatal(err)
	}

	cost := 0.25
	if err := s.RecordUsage("t1", Usage{
		Model: "claude-opus-5", InputTokens: 100, CacheWriteTokens: 200,
		CacheReadTokens: 90000, OutputTokens: 50, Known: true,
		ComputedUSD: &cost, PriceTableVer: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTurn("t1", TurnDone, "", 1500, 3); err != nil {
		t.Fatal(err)
	}

	rows, err := s.CostByRunner(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	// Collapsing these is the single easiest way to be wrong by an order of
	// magnitude, so the store keeps them apart all the way through.
	if r.Input != 100 || r.CacheWrite != 200 || r.CacheRead != 90000 || r.Output != 50 {
		t.Fatalf("counters collapsed: %+v", r)
	}
	if r.CostUSD != 0.25 {
		t.Errorf("cost = %v", r.CostUSD)
	}
	if r.UnknownTurns != 0 {
		t.Errorf("unknown = %d, want 0", r.UnknownTurns)
	}
}

func TestUnknownUsageCountedSeparately(t *testing.T) {
	s := open(t)
	if _, _, err := s.BindThread("k1", "slack", "C1", "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2"} {
		if err := s.StartTurn(Turn{ID: id, ThreadID: "k1", Channel: "C1", Runner: "alpha", SurfaceUser: "U1"}); err != nil {
			t.Fatal(err)
		}
	}
	cost := 1.0
	_ = s.RecordUsage("t1", Usage{Model: "m", InputTokens: 10, Known: true, ComputedUSD: &cost})
	_ = s.RecordUsage("t2", Usage{Known: false})

	rows, err := s.CostByRunner(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].UnknownTurns != 1 {
		t.Fatalf("unknown turns = %d, want 1 — unmetered work must not read as free",
			rows[0].UnknownTurns)
	}
	if rows[0].Turns != 2 {
		t.Fatalf("turns = %d, want 2", rows[0].Turns)
	}
}

func TestQueueRoundTrip(t *testing.T) {
	s := open(t)

	if err := s.Enqueue("alpha", "k1", []byte(`{"t":"message"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("alpha", "k1", []byte(`{"t":"message","n":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("beta", "k2", []byte(`{"t":"message"}`)); err != nil {
		t.Fatal(err)
	}

	depth, err := s.QueueDepth("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 2 {
		t.Fatalf("depth = %d, want 2", depth)
	}

	msgs, err := s.Dequeue("alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("dequeued %d", len(msgs))
	}
	// FIFO: replaying out of order would reorder a conversation.
	if string(msgs[0].Frame) != `{"t":"message"}` {
		t.Errorf("first frame = %s", msgs[0].Frame)
	}
	for _, m := range msgs {
		if err := s.DeleteQueued(m.ID); err != nil {
			t.Fatal(err)
		}
	}
	if d, _ := s.QueueDepth("alpha"); d != 0 {
		t.Errorf("depth after drain = %d", d)
	}
	// Another runner's queue is untouched.
	if d, _ := s.QueueDepth("beta"); d != 1 {
		t.Errorf("beta depth = %d, want 1", d)
	}
}

func TestAuditWrites(t *testing.T) {
	s := open(t)

	if err := s.Log(Event{Kind: "runner.connected", Runner: "alpha",
		Detail: map[string]any{"host": "i-123"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPermissionRequest("r1", "k1", "t1", "alpha", "Bash", `{"command":"ls"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPermissionDecision("r1", "allow", "U1", "", false); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCredential(CredentialRecord{
		RequestID: "c1", Runner: "alpha", Kind: "forge", Resource: "acme/widgets", Granted: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMCPCall(MCPRecord{
		CallID: "m1", Runner: "alpha", Server: "jira", Tool: "get_issue", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Blob records upsert, so a completion can update an in-flight row.
	if err := s.RecordBlob(BlobRecord{ID: "b1", Direction: "inbound", Name: "x.csv"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordBlob(BlobRecord{ID: "b1", Direction: "inbound", Name: "x.csv", OK: true}); err != nil {
		t.Fatal(err)
	}
}

func TestLogRejectsUnmarshalableDetail(t *testing.T) {
	s := open(t)
	// A gateway that cannot audit should be visibly broken, not quietly lossy.
	if err := s.Log(Event{Kind: "x", Detail: make(chan int)}); err == nil {
		t.Fatal("expected an error for an unmarshalable detail")
	}
}

func TestFailOpenTurnsClosesOnlyRunning(t *testing.T) {
	s := open(t)
	if _, _, err := s.BindThread("k1", "slack", "C1", "alpha"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := s.StartTurn(Turn{ID: id, ThreadID: "k1", Channel: "C1", Runner: "alpha", SurfaceUser: "U1"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishTurn("t1", TurnDone, "", 100, 2); err != nil {
		t.Fatal(err)
	}

	n, err := s.FailOpenTurns("gateway_restart")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 open turns closed, got %d", n)
	}

	var done, errored int
	if err := s.db.QueryRow(`SELECT count(*) FROM turns WHERE status = ?`, TurnDone).Scan(&done); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM turns WHERE status = ? AND error = 'gateway_restart' AND ended_at IS NOT NULL`, TurnError).Scan(&errored); err != nil {
		t.Fatal(err)
	}
	if done != 1 || errored != 2 {
		t.Fatalf("done=%d errored=%d, want 1 and 2", done, errored)
	}

	// Idempotent: a second sweep finds nothing.
	if n, err := s.FailOpenTurns("gateway_restart"); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
}
