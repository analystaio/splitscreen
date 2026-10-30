package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/protocol"
)

// sendFileAgainst connects a runner to a fake gateway speaking gwProto and
// starts a send-file into a running turn. It returns the result channel.
func sendFileAgainst(t *testing.T, gwProto string) (*fakeGateway, *websocket.Conn, chan error) {
	t.Helper()
	g := newFakeGateway(t)
	r, ws := connectedRunner(t, g)
	g.reply(ws, &protocol.HelloAck{Protocol: gwProto, Runner: "alpha", Routes: []string{"C1"}})

	want := int32(0)
	if v, _ := protocol.ParseVersion(gwProto); v.Major == protocol.Ours().Major {
		want = int32(v.Minor)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.gatewayMinor.Load() != want {
		if time.Now().After(deadline) {
			t.Fatal("hello_ack was never applied")
		}
		time.Sleep(5 * time.Millisecond)
	}

	sess := &threadSession{threadID: "thread-1"}
	sess.setTurn("turn-1")
	r.sessions.Store("thread-1", sess)

	path := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.sendFile(context.Background(), "thread-1", path, "") }()
	return g, ws, done
}

func result(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("send-file never returned")
		return nil
	}
}

// A file the gateway could not deliver used to read as "sent". Against a
// gateway that sends receipts, its failure must reach the caller.
func TestSendFileReportsAFailedDelivery(t *testing.T) {
	g, ws, done := sendFileAgainst(t, protocol.Version)
	end, ok := await[*protocol.BlobEnd](g)
	if !ok {
		t.Fatal("no blob.end")
	}
	g.reply(ws, &protocol.BlobReceipt{BlobID: end.BlobID, Error: "no live turn to deliver to"})
	err := result(t, done)
	if err == nil || !strings.Contains(err.Error(), "no live turn") {
		t.Fatalf("want the gateway's reason, got %v", err)
	}
}

func TestSendFileWaitsForTheReceipt(t *testing.T) {
	g, ws, done := sendFileAgainst(t, protocol.Version)
	end, ok := await[*protocol.BlobEnd](g)
	if !ok {
		t.Fatal("no blob.end")
	}
	select {
	case err := <-done:
		t.Fatalf("send-file returned before the receipt: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	g.reply(ws, &protocol.BlobReceipt{BlobID: end.BlobID, OK: true})
	if err := result(t, done); err != nil {
		t.Fatalf("send-file: %v", err)
	}
}

// An older gateway never sends a receipt; waiting for one would stall every
// send-file until it timed out.
func TestSendFileToAnOlderGatewayDoesNotWait(t *testing.T) {
	g, _, done := sendFileAgainst(t, "1.3")
	if _, ok := await[*protocol.BlobEnd](g); !ok {
		t.Fatal("no blob.end")
	}
	if err := result(t, done); err != nil {
		t.Fatalf("send-file: %v", err)
	}
}

func TestSendFileGivesUpOnAMissingReceipt(t *testing.T) {
	old := blobReceiptTimeout
	blobReceiptTimeout = 100 * time.Millisecond
	t.Cleanup(func() { blobReceiptTimeout = old })

	_, _, done := sendFileAgainst(t, protocol.Version)
	err := result(t, done)
	if err == nil || !strings.Contains(err.Error(), "no delivery receipt") {
		t.Fatalf("want a missing-receipt error, got %v", err)
	}
}
