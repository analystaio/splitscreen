// Package runner is the agent-side daemon. It owns a working tree and a harness
// and holds no credentials at rest: routing, policy, and every secret live on
// the gateway.
package runner

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/avarant/splitscreen/internal/harness"
	"github.com/avarant/splitscreen/protocol"
)

// Options configure a runner.
type Options struct {
	Name    string
	Gateway string // wss://host:port
	Token   string
	// Fingerprint pins the gateway's self-signed certificate. Public CAs do not
	// issue for private addresses, and a private CA is disproportionate for a
	// small fleet.
	Fingerprint string
	Insecure    bool

	Cwd         string
	HarnessName string
	// HarnessCredentials is an existing credentials file on this host to expose
	// inside the materialized config directory. Subscription-authenticated
	// harnesses keep their credentials on disk and refresh them in place, so the
	// gateway has nothing to ship — the file is already here and must simply be
	// reachable from the config dir the harness is pointed at.
	HarnessCredentials string
	RuntimeRoot        string
	SelfPath           string
	IdleTimeout        time.Duration
	// MaxSessions caps resident harness processes; 0 means unlimited. When a
	// new session would exceed it, the longest-idle session between turns is
	// reaped first, and the new session is refused if none is evictable.
	MaxSessions int

	Logger *slog.Logger
}

// Runner is the daemon.
type Runner struct {
	opts    Options
	log     *slog.Logger
	adapter harness.Adapter

	bundle materialized
	ipc    *ipcServer

	connMu sync.RWMutex
	conn   *websocket.Conn
	sendMu sync.Mutex

	sessions sync.Map // thread id -> *threadSession
	pending  sync.Map // request id -> chan any
	blobs    sync.Map // blob id -> *inboundBlob

}

// New builds a runner.
func New(o Options) (*Runner, error) {
	if o.Name == "" || o.Gateway == "" {
		return nil, errors.New("runner: name and gateway are required")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if o.RuntimeRoot == "" {
		o.RuntimeRoot = DefaultRuntimeRoot()
	}
	if o.HarnessName == "" {
		o.HarnessName = "claude-code"
	}
	if o.SelfPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("runner: locate own binary: %w", err)
		}
		o.SelfPath = exe
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 30 * time.Minute
	}
	a, err := harness.Get(o.HarnessName)
	if err != nil {
		return nil, err
	}
	return &Runner{opts: o, log: o.Logger, adapter: a}, nil
}

// DefaultRuntimeRoot prefers tmpfs so nothing lands on persistent disk.
func DefaultRuntimeRoot() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir + "/splitscreen"
	}
	return "/run/splitscreen"
}

// Run connects and serves until ctx is cancelled, reconnecting with backoff.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.startIPC(); err != nil {
		return err
	}
	defer r.ipc.close()

	go r.sweepIdle(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		err := r.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		r.log.Warn("gateway connection ended", "err", err, "retry_in", backoff)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > time.Minute {
			backoff = time.Minute
		}
	}
	return nil
}

func jitter(d time.Duration) time.Duration {
	// Full jitter: a fleet restarting together should not stampede the gateway.
	return time.Duration(rand.Int63n(int64(d)) + int64(d)/2)
}

func (r *Runner) connectOnce(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpClient := &http.Client{}
	if strings.HasPrefix(r.opts.Gateway, "wss://") {
		tlsCfg, err := r.tlsConfig()
		if err != nil {
			return err
		}
		httpClient.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}

	ws, _, err := websocket.Dial(dialCtx, strings.TrimRight(r.opts.Gateway, "/")+"/runner",
		&websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return fmt.Errorf("runner: dial gateway: %w", err)
	}
	ws.SetReadLimit(protocol.MaxControlFrame + protocol.MaxChunk + 4096)
	defer ws.Close(websocket.StatusNormalClosure, "shutting down")

	r.connMu.Lock()
	r.conn = ws
	r.connMu.Unlock()
	defer func() {
		r.connMu.Lock()
		r.conn = nil
		r.connMu.Unlock()
		r.failPending("gateway connection lost")
	}()

	hello := &protocol.Hello{
		Protocol: protocol.Version,
		Runner:   r.opts.Name,
		Auth:     protocol.Auth{Mode: protocol.AuthToken, Value: r.opts.Token},
		Host: protocol.Host{
			ID: hostID(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		},
		Harness:      protocol.HarnessInfo{Adapter: r.adapter.Name()},
		Capabilities: []string{"files", "images", "permission-prompt-tool", "proxied-mcp"},
	}
	// Tell the gateway what we already have. After a reboot this is empty even
	// though the gateway may believe it pushed — the runtime root is tmpfs.
	if v, d := r.bundle.Version(), r.bundle.Digest(); d != "" {
		hello.Bundle = &protocol.BundleRef{Version: v, Digest: d}
	}
	if err := r.send(ctx, hello); err != nil {
		return err
	}
	r.log.Info("connected to gateway", "gateway", r.opts.Gateway, "runner", r.opts.Name)

	return r.readLoop(ctx, ws)
}

func (r *Runner) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case r.opts.Fingerprint != "":
		want := normalizeFingerprint(r.opts.Fingerprint)
		// Pinning replaces chain validation: the gateway's certificate is
		// self-signed by design, so a public CA has nothing to say about it.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				sum := sha256.Sum256(raw)
				if hex.EncodeToString(sum[:]) == want {
					return nil
				}
			}
			return errors.New("runner: gateway certificate does not match the pinned fingerprint")
		}
	case r.opts.Insecure:
		r.log.Warn("TLS verification disabled; use a pinned fingerprint outside development")
		cfg.InsecureSkipVerify = true
	}
	return cfg, nil
}

func normalizeFingerprint(fp string) string {
	fp = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fp)), "sha256:")
	return strings.ReplaceAll(fp, ":", "")
}

func hostID() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

func (r *Runner) send(ctx context.Context, f protocol.Frame) error {
	data, err := protocol.Encode(f)
	if err != nil {
		return err
	}
	return r.write(ctx, websocket.MessageText, data)
}

func (r *Runner) write(ctx context.Context, typ websocket.MessageType, data []byte) error {
	r.connMu.RLock()
	ws := r.conn
	r.connMu.RUnlock()
	if ws == nil {
		return errors.New("runner: not connected to the gateway")
	}
	// coder/websocket permits one writer at a time.
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ws.Write(wctx, typ, data)
}

// writeBinary sends a chunk frame.
func (r *Runner) writeBinary(ctx context.Context, data []byte) error {
	return r.write(ctx, websocket.MessageBinary, data)
}

func (r *Runner) readLoop(ctx context.Context, ws *websocket.Conn) error {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageText:
			frame, derr := protocol.Decode(data, protocol.DirDown)
			if derr != nil {
				if errors.Is(derr, protocol.ErrUnknownFrame) {
					continue // additive change from a newer gateway minor
				}
				r.log.Warn("bad frame from gateway", "err", derr)
				continue
			}
			r.handleFrame(ctx, frame)
		case websocket.MessageBinary:
			hdr, payload, derr := protocol.DecodeChunk(data)
			if derr != nil {
				r.log.Warn("bad chunk from gateway", "err", derr)
				continue
			}
			r.onChunk(hdr, payload)
		}
	}
}

func (r *Runner) handleFrame(ctx context.Context, f protocol.Frame) {
	switch fr := f.(type) {
	case *protocol.Ping:
		_ = r.send(ctx, &protocol.Pong{Nonce: fr.Nonce})
	case *protocol.Pong:
	case *protocol.HelloAck:
		r.log.Info("registered with gateway",
			"runner", fr.Runner, "routes", fr.Routes, "bundle", fr.Bundle.Version)
	case *protocol.BundlePush:
		if err := r.applyBundle(fr); err != nil {
			r.log.Error("bundle apply failed", "err", err)
			_ = r.send(ctx, &protocol.Error{
				Code: "bundle_apply_failed", Message: err.Error(),
			})
			return
		}
		r.preflightMCP(ctx, fr)
	case *protocol.Message:
		go r.handleMessage(context.WithoutCancel(ctx), fr)
	case *protocol.PermissionResponse:
		r.resolvePending(fr.RequestID, fr)
	case *protocol.CredentialGrant:
		r.resolvePending(fr.RequestID, fr)
	case *protocol.MCPResponse:
		r.resolvePending(fr.CallID, fr)
	case *protocol.BlobBegin:
		r.onBlobBegin(fr)
	case *protocol.BlobEnd:
		r.onBlobEnd(fr)
	case *protocol.Error:
		r.log.Error("gateway reported an error", "code", fr.Code, "message", fr.Message)
	default:
		r.log.Warn("unhandled frame", "type", f.Type())
	}
}

// resolvePending hands a reply to whoever is waiting on it.
func (r *Runner) resolvePending(id string, v any) {
	ch, ok := r.pending.LoadAndDelete(id)
	if !ok {
		return
	}
	select {
	case ch.(chan any) <- v:
	default:
	}
}

// waitFor registers a waiter and blocks for the reply.
func (r *Runner) waitFor(ctx context.Context, id string) (any, error) {
	ch := make(chan any, 1)
	r.pending.Store(id, ch)
	defer r.pending.Delete(id)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case v := <-ch:
		return v, nil
	}
}

// failPending unblocks every waiter when the connection drops. Leaving them to
// time out would stall a harness for as long as its own deadline allows.
func (r *Runner) failPending(reason string) {
	r.pending.Range(func(key, value any) bool {
		r.pending.Delete(key)
		select {
		case value.(chan any) <- errors.New(reason):
		default:
		}
		return true
	})
}

// preflightMCP reports declared-but-missing servers. A missing dependency should
// read as a runner capability error, not as the agent mysteriously lacking a
// tool.
func (r *Runner) preflightMCP(ctx context.Context, push *protocol.BundlePush) {
	var missing []string
	for _, s := range push.MCP {
		if s.Kind != protocol.MCPLocal || s.Command == "" {
			continue
		}
		if _, err := exec.LookPath(s.Command); err != nil {
			missing = append(missing, s.Name+" ("+s.Command+")")
		}
	}
	if len(missing) == 0 {
		return
	}
	msg := "declared but not installed: " + strings.Join(missing, ", ")
	r.log.Error("mcp preflight failed", "missing", missing)
	_ = r.send(ctx, &protocol.Error{Code: "mcp_missing", Message: msg})
}

// Bundle exposes the materialized state for status output.
func (r *Runner) Bundle() (version int, digest string) {
	return r.bundle.Version(), r.bundle.Digest()
}
