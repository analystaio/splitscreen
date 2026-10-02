package runner

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/analystaio/splitscreen/internal/mcpproxy"
	"github.com/analystaio/splitscreen/protocol"
)

// Everything in this file is a request the runner forwards to the gateway.
// None of it is decided locally: the runner holds no credentials and enforces
// no policy, which is what makes the gateway's checks unbypassable from here.

// CredentialResult is what the git credential helper needs.
type CredentialResult struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// requestCredential asks the gateway to mint a repository-scoped git credential.
func (r *Runner) requestCredential(ctx context.Context, resource string) (CredentialResult, error) {
	if resource == "" {
		return CredentialResult{}, errors.New("runner: a repository is required")
	}
	id := "cred_" + randomID()
	req := &protocol.CredentialRequest{
		RequestID: id,
		Kind:      protocol.CredentialForge,
		Resource:  resource,
	}
	v, err := r.request(ctx, id, req)
	if err != nil {
		return CredentialResult{}, err
	}
	switch reply := v.(type) {
	case error:
		return CredentialResult{}, reply
	case *protocol.CredentialGrant:
		if reply.Denied {
			// Surface the reason rather than an empty credential: an opaque auth
			// failure is much harder to act on than "policy refused this repo".
			return CredentialResult{}, fmt.Errorf("refused by gateway policy: %s", reply.Reason)
		}
		return CredentialResult{Username: reply.Username, Password: reply.Value}, nil
	default:
		return CredentialResult{}, fmt.Errorf("runner: unexpected reply %T", v)
	}
}

// PermissionResult is the permission-prompt tool's answer.
type PermissionResult struct {
	Behavior string `json:"behavior"` // allow | deny
	Message  string `json:"message,omitempty"`
}

// requestPermission routes a tool decision to the gateway, which applies policy
// before any human is asked and records who decided.
func (r *Runner) requestPermission(ctx context.Context, threadID, tool string, input json.RawMessage) (PermissionResult, error) {
	if tool == "" {
		return PermissionResult{}, errors.New("runner: a tool name is required")
	}
	id := "perm_" + randomID()
	req := &protocol.PermissionRequest{
		ThreadID:  threadID,
		TurnID:    r.turnForThread(threadID),
		RequestID: id,
		Tool:      tool,
		Input:     input,
		Cwd:       r.opts.Cwd,
	}
	v, err := r.request(ctx, id, req)
	if err != nil {
		// A timeout or a dropped connection must not read as approval.
		return PermissionResult{Behavior: "deny", Message: "no decision reached the runner"}, nil
	}
	switch reply := v.(type) {
	case error:
		return PermissionResult{Behavior: "deny", Message: reply.Error()}, nil
	case *protocol.PermissionResponse:
		if reply.Decision == protocol.DecisionDeny {
			msg := reply.Reason
			if msg == "" {
				msg = "denied"
			}
			return PermissionResult{Behavior: "deny", Message: msg}, nil
		}
		return PermissionResult{Behavior: "allow"}, nil
	default:
		return PermissionResult{Behavior: "deny", Message: "unexpected reply"}, nil
	}
}

// callProxiedMCP forwards a tool call for a credential-bearing server. The
// credential never reaches this process.
func (r *Runner) callProxiedMCP(ctx context.Context, threadID, server, tool string, args json.RawMessage) (json.RawMessage, error) {
	id := "mcp_" + randomID()
	req := &protocol.MCPCall{
		ThreadID: threadID,
		TurnID:   r.turnForThread(threadID),
		CallID:   id,
		Server:   server,
		Tool:     tool,
		Args:     args,
	}
	v, err := r.request(ctx, id, req)
	if err != nil {
		return nil, err
	}
	switch reply := v.(type) {
	case error:
		return nil, reply
	case *protocol.MCPResponse:
		if reply.Error != nil {
			return nil, fmt.Errorf("%s: %s", reply.Error.Code, reply.Error.Message)
		}
		return reply.Result, nil
	default:
		return nil, fmt.Errorf("runner: unexpected reply %T", v)
	}
}

// listProxiedMCP asks the gateway for a proxied server's tool list. Discovery
// rides on the same frame as invocation so that every interaction with a
// credentialed server has one audited path.
func (r *Runner) listProxiedMCP(ctx context.Context, threadID, server string) (json.RawMessage, error) {
	return r.callProxiedMCP(ctx, threadID, server, mcpproxy.ListTool, nil)
}

// sendFile streams a local file to the surface through the gateway.
func (r *Runner) sendFile(ctx context.Context, threadID, path, comment string) error {
	if path == "" {
		return errors.New("runner: a path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("runner: %s is a directory", path)
	}
	if info.Size() > protocol.MaxBlobSize {
		return fmt.Errorf("runner: %s is %d bytes, over the %d cap",
			path, info.Size(), protocol.MaxBlobSize)
	}

	blobID := "blob_" + randomID()
	begin := &protocol.BlobBegin{
		BlobID:   blobID,
		ThreadID: threadID,
		TurnID:   r.turnForThread(threadID),
		Name:     filepath.Base(path),
		Mime:     guessMime(path),
		Size:     info.Size(),
	}
	if err := r.send(ctx, begin); err != nil {
		return err
	}

	hash := sha256.New()
	buf := make([]byte, protocol.MaxChunk)
	var seq uint32
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
			chunk, cerr := protocol.EncodeChunk(protocol.ChunkHeader{BlobID: blobID, Seq: seq}, buf[:n])
			if cerr != nil {
				return cerr
			}
			if werr := r.writeBinary(ctx, chunk); werr != nil {
				return werr
			}
			seq++
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_ = r.send(ctx, &protocol.BlobEnd{BlobID: blobID, OK: false, Error: rerr.Error()})
			return rerr
		}
	}
	return r.send(ctx, &protocol.BlobEnd{
		BlobID: blobID, SHA256: hex.EncodeToString(hash.Sum(nil)), OK: true,
	})
}

func guessMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	case ".txt", ".log", ".md":
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// ---------------------------------------------------------------------------
// Inbound blobs
// ---------------------------------------------------------------------------

type inboundBlob struct {
	mu   sync.Mutex
	meta *protocol.BlobBegin
	file *os.File
	path string
	hash interface {
		io.Writer
		Sum([]byte) []byte
	}
	failed bool
	// complete is set only once blob.end has arrived and verified. A transfer
	// interrupted by a gateway restart would otherwise be handed to the harness
	// as a silently truncated file.
	complete bool
}

func (r *Runner) onBlobBegin(fr *protocol.BlobBegin) {
	dir := filepath.Join(r.threadDir(fr.ThreadID), "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		r.log.Error("upload dir creation failed", "err", err)
		return
	}
	// The name was sanitized by the gateway; sanitizing again here means the
	// guarantee does not depend on the peer behaving.
	name := sanitizeName(fr.Name)
	dest := filepath.Join(dir, name)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		r.log.Error("upload file creation failed", "err", err)
		return
	}
	r.blobs.Store(fr.BlobID, &inboundBlob{
		meta: fr, file: f, path: dest, hash: sha256.New(),
	})
}

func (r *Runner) onChunk(hdr protocol.ChunkHeader, payload []byte) {
	v, ok := r.blobs.Load(hdr.BlobID)
	if !ok {
		return
	}
	b := v.(*inboundBlob)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		return
	}
	if _, err := b.file.Write(payload); err != nil {
		r.log.Error("upload write failed", "err", err)
		b.failed = true
		return
	}
	_, _ = b.hash.Write(payload)
}

func (r *Runner) onBlobEnd(fr *protocol.BlobEnd) {
	v, ok := r.blobs.Load(fr.BlobID)
	if !ok {
		return
	}
	b := v.(*inboundBlob)
	b.mu.Lock()
	defer b.mu.Unlock()
	_ = b.file.Close()

	if !fr.OK || b.failed {
		_ = os.Remove(b.path)
		r.blobs.Delete(fr.BlobID)
		return
	}
	if fr.SHA256 != "" {
		got := hex.EncodeToString(b.hash.Sum(nil))
		if got != fr.SHA256 {
			r.log.Error("attachment checksum mismatch", "blob", fr.BlobID)
			_ = os.Remove(b.path)
			r.blobs.Delete(fr.BlobID)
			return
		}
	}
	b.complete = true
}

// takeBlob claims a delivered attachment, returning its bytes if it is small
// enough to inline and always its path.
func (r *Runner) takeBlob(blobID string) (data []byte, path string, ok bool) {
	v, loaded := r.blobs.Load(blobID)
	if !loaded {
		return nil, "", false
	}
	b := v.(*inboundBlob)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed || !b.complete {
		// Leave it in place: a transfer still in flight will complete, and
		// consuming the entry here would make it unclaimable afterwards.
		return nil, "", false
	}
	r.blobs.Delete(blobID)
	// Only inline what is small enough to belong in a prompt; everything else is
	// handed over as a path.
	const inlineCap = 5 << 20
	if b.meta.Size > 0 && b.meta.Size <= inlineCap {
		if content, err := os.ReadFile(b.path); err == nil {
			return content, b.path, true
		}
	}
	return nil, b.path, true
}

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

func randomID() string {
	var b [10]byte
	if _, err := readRandom(b[:]); err != nil {
		panic("runner: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func readRandom(b []byte) (int, error) { return crand.Read(b) }
