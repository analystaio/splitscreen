package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// upload streams body to the gateway as a runner's send-file would.
func upload(t *testing.T, ws *websocket.Conn, blobID, threadID, turnID, body string) {
	t.Helper()
	send(t, ws, &protocol.BlobBegin{BlobID: blobID, ThreadID: threadID, TurnID: turnID,
		Name: "shot.png", Mime: "image/png", Size: int64(len(body))})
	chunk, err := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: blobID}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, chunk); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	send(t, ws, &protocol.BlobEnd{BlobID: blobID, SHA256: hex.EncodeToString(sum[:]), OK: true})
}

func startTurn(t *testing.T, h *harness, ws *websocket.Conn) *protocol.Message {
	t.Helper()
	readFrame[*protocol.HelloAck](t, ws)
	h.gw.OnMessage(context.Background(), surface.Inbound{
		Surface: "test", Channel: "C1", Thread: "T1",
		User: surface.User{ID: "U1"}, Text: "send me a screenshot", Addressed: true,
	})
	return readFrame[*protocol.Message](t, ws)
}

func TestDeliveredUploadIsReceipted(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	msg := startTurn(t, h, ws)

	upload(t, ws, "blob_ok", msg.ThreadID, msg.TurnID, "png")
	rc := readFrame[*protocol.BlobReceipt](t, ws)
	if rc.BlobID != "blob_ok" || !rc.OK {
		t.Fatalf("receipt = %+v", rc)
	}
	h.srf.mu.Lock()
	defer h.srf.mu.Unlock()
	if len(h.srf.uploads) != 1 || h.srf.uploads[0] != "shot.png:png" {
		t.Fatalf("uploads = %v", h.srf.uploads)
	}
}

// The failure behind the original report: a file for a turn the gateway does
// not know was recorded as undelivered while the runner told its caller "sent".
func TestUndeliverableUploadIsReceiptedAsFailed(t *testing.T) {
	h := newHarness(t)
	ws := h.connect(t, "s3cret")
	msg := startTurn(t, h, ws)

	upload(t, ws, "blob_lost", msg.ThreadID, "turn_unknown", "png")
	rc := readFrame[*protocol.BlobReceipt](t, ws)
	if rc.OK || rc.Error != "no live turn to deliver to" {
		t.Fatalf("receipt = %+v", rc)
	}
}

// A runner older than the receipt would drop the frame as unknown; it is not
// sent one at all.
func TestOlderRunnerGetsNoReceipt(t *testing.T) {
	h := newHarness(t)
	ws := h.connectSpeaking(t, "s3cret", "1.3")
	msg := startTurn(t, h, ws)

	upload(t, ws, "blob_old", msg.ThreadID, msg.TurnID, "png")
	eventually(t, "upload", func() bool {
		h.srf.mu.Lock()
		defer h.srf.mu.Unlock()
		return len(h.srf.uploads) == 1
	})
	send(t, ws, &protocol.Ping{Nonce: 42})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		f, err := protocol.Decode(data, protocol.DirDown)
		if err != nil {
			continue
		}
		switch fr := f.(type) {
		case *protocol.BlobReceipt:
			t.Fatalf("a 1.3 runner was sent %+v", fr)
		case *protocol.Pong:
			if fr.Nonce == 42 {
				return // the gateway handled everything before the ping
			}
		}
	}
}
