package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// relayFilesToRunner downloads attachments with the surface credential and
// streams them to the runner over the existing connection.
//
// The runner holds no chat platform token, so it cannot fetch these itself. This
// is also the only place inbound files are seen, which is what makes the audit
// record complete.
func (g *Gateway) relayFilesToRunner(ctx context.Context, c *Conn, turn *turnContext, files []surface.File) ([]protocol.Attachment, error) {
	var out []protocol.Attachment
	for _, f := range files {
		att, err := g.relayOne(ctx, c, turn, f)
		if err != nil {
			return out, err
		}
		out = append(out, att)
	}
	return out, nil
}

func (g *Gateway) relayOne(ctx context.Context, c *Conn, turn *turnContext, f surface.File) (protocol.Attachment, error) {
	if f.Size > protocol.MaxBlobSize {
		return protocol.Attachment{}, fmt.Errorf("%q is %d bytes, over the %d cap",
			f.Name, f.Size, protocol.MaxBlobSize)
	}
	blobID := newID("blob")
	name := sanitizeName(f.Name)

	begin := &protocol.BlobBegin{
		BlobID: blobID, ThreadID: turn.ThreadID, TurnID: turn.TurnID,
		Name: name, Mime: f.Mime, Size: f.Size,
	}
	if err := begin.Validate(); err != nil {
		return protocol.Attachment{}, err
	}
	if err := c.Send(begin); err != nil {
		return protocol.Attachment{}, err
	}

	rc, err := f.Open(ctx)
	if err != nil {
		_ = c.Send(&protocol.BlobEnd{BlobID: blobID, OK: false, Error: err.Error()})
		return protocol.Attachment{}, err
	}
	defer rc.Close()

	hash := sha256.New()
	buf := make([]byte, protocol.MaxChunk)
	var seq uint32
	var total int64
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > protocol.MaxBlobSize {
				_ = c.Send(&protocol.BlobEnd{BlobID: blobID, OK: false, Error: "exceeded size cap mid-transfer"})
				return protocol.Attachment{}, fmt.Errorf("%q exceeded the size cap mid-transfer", name)
			}
			hash.Write(buf[:n])
			chunk, cerr := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: blobID, Seq: seq}, buf[:n])
			if cerr != nil {
				return protocol.Attachment{}, cerr
			}
			if err := c.sendBinary(chunk); err != nil {
				return protocol.Attachment{}, err
			}
			seq++
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = c.Send(&protocol.BlobEnd{BlobID: blobID, OK: false, Error: rerr.Error()})
			return protocol.Attachment{}, rerr
		}
	}

	sum := hex.EncodeToString(hash.Sum(nil))
	if err := c.Send(&protocol.BlobEnd{BlobID: blobID, SHA256: sum, OK: true}); err != nil {
		return protocol.Attachment{}, err
	}

	if err := g.store.RecordBlob(store.BlobRecord{
		ID: blobID, Direction: "inbound", ThreadID: turn.ThreadID, TurnID: turn.TurnID,
		Runner: turn.Runner, Name: name, Mime: f.Mime, Size: total, SHA256: sum,
		SurfaceUser: turn.User.ID, OK: true,
	}); err != nil {
		g.log.Error("record blob failed", "err", err)
	}

	return protocol.Attachment{
		BlobID: blobID, Name: name, Mime: f.Mime, Size: total, SHA256: sum,
		Inline: strings.HasPrefix(f.Mime, "image/"),
	}, nil
}

// sanitizeName reduces an attachment name to a bare filename. Both peers check
// this, so the guarantee does not rest on either implementation alone.
func sanitizeName(n string) string {
	n = strings.TrimSpace(n)
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.ReplaceAll(n, "\x00", "")
	if n == "" || n == "." || n == ".." {
		return "attachment"
	}
	return n
}

// ---------------------------------------------------------------------------
// Runner -> surface
// ---------------------------------------------------------------------------

// incomingBlob assembles a transfer from a runner. Bytes are spooled to a temp
// file rather than held in memory: a runner can legitimately send something
// large, and the gateway is a shared process.
type incomingBlob struct {
	mu     sync.Mutex
	meta   *protocol.BlobBegin
	file   *os.File
	hash   interface{ Write([]byte) (int, error) }
	sum    interface{ Sum([]byte) []byte }
	closed bool
	turn   *turnContext
}

func (g *Gateway) onBlobBegin(c *Conn, fr *protocol.BlobBegin) {
	tmp, err := os.CreateTemp("", "splitscreen-upload-*")
	if err != nil {
		g.log.Error("temp file for upload failed", "err", err)
		_ = c.Send(&protocol.BlobEnd{BlobID: fr.BlobID, OK: false, Error: err.Error()})
		return
	}
	h := sha256.New()
	turn, _ := g.turnFor(fr.TurnID)
	c.blobs.Store(fr.BlobID, &incomingBlob{
		meta: fr, file: tmp, hash: h, sum: h, turn: turn,
	})
}

func (g *Gateway) onChunk(c *Conn, hdr protocol.ChunkHeader, payload []byte) {
	v, ok := c.blobs.Load(hdr.BlobID)
	if !ok {
		g.log.Warn("chunk for unknown blob", "runner", c.runner, "blob", hdr.BlobID)
		return
	}
	b := v.(*incomingBlob)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if _, err := b.file.Write(payload); err != nil {
		g.log.Error("upload spool write failed", "err", err)
		b.closed = true
		return
	}
	_, _ = b.hash.Write(payload)
}

func (g *Gateway) onBlobEnd(ctx context.Context, c *Conn, fr *protocol.BlobEnd) {
	v, ok := c.blobs.LoadAndDelete(fr.BlobID)
	if !ok {
		return
	}
	b := v.(*incomingBlob)
	b.mu.Lock()
	defer b.mu.Unlock()

	path := b.file.Name()
	defer func() {
		_ = b.file.Close()
		_ = os.Remove(path)
	}()

	rec := store.BlobRecord{
		ID: fr.BlobID, Direction: "outbound", Runner: c.runner,
		Name: sanitizeName(b.meta.Name), Mime: b.meta.Mime,
	}
	if b.turn != nil {
		rec.ThreadID = b.turn.ThreadID
		rec.TurnID = b.turn.TurnID
		rec.SurfaceUser = b.turn.User.ID
	}

	if !fr.OK {
		rec.OK = false
		rec.Error = fr.Error
		_ = g.store.RecordBlob(rec)
		g.log.Warn("runner reported a failed transfer", "blob", fr.BlobID, "err", fr.Error)
		return
	}

	sum := hex.EncodeToString(b.sum.Sum(nil))
	if fr.SHA256 != "" && fr.SHA256 != sum {
		rec.OK = false
		rec.Error = "checksum mismatch"
		_ = g.store.RecordBlob(rec)
		g.log.Error("upload checksum mismatch", "blob", fr.BlobID, "want", fr.SHA256, "got", sum)
		return
	}
	rec.SHA256 = sum

	info, err := b.file.Stat()
	if err == nil {
		rec.Size = info.Size()
	}

	if b.turn == nil {
		rec.OK = false
		rec.Error = "no live turn to deliver to"
		_ = g.store.RecordBlob(rec)
		return
	}
	srf, ok := g.surfaceFor(b.turn.Surface)
	if !ok {
		rec.OK = false
		rec.Error = "surface unavailable"
		_ = g.store.RecordBlob(rec)
		return
	}

	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		rec.OK = false
		rec.Error = err.Error()
		_ = g.store.RecordBlob(rec)
		return
	}

	if err := srf.Upload(ctx, surface.Upload{
		Channel: b.turn.Channel,
		Thread:  b.turn.Thread,
		Name:    rec.Name,
		Mime:    rec.Mime,
		Content: b.file,
		Size:    rec.Size,
	}); err != nil {
		rec.OK = false
		rec.Error = err.Error()
		g.log.Error("surface upload failed", "err", err)
	} else {
		rec.OK = true
	}
	_ = g.store.RecordBlob(rec)
	b.closed = true
}
