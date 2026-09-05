package gateway

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/avarant/splitscreen/config"

	"github.com/avarant/splitscreen/internal/store"
	"github.com/avarant/splitscreen/protocol"
)

// ServeRunners starts the runner-facing WebSocket listener and blocks until ctx
// is cancelled.
//
// Runners dial out; the gateway never dials a runner. That is what lets runners
// live in private subnets, behind NAT, or on laptops with no inbound rules.
func (g *Gateway) ServeRunners(ctx context.Context) error {
	c := g.cfg.Load()

	mux := http.NewServeMux()
	mux.HandleFunc("/runner", g.handleRunner)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	srv := &http.Server{
		Addr:              c.Gateway.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	if c.Gateway.TLS.Cert != "" {
		cert, err := tls.LoadX509KeyPair(c.Gateway.TLS.Cert, c.Gateway.TLS.Key)
		if err != nil {
			return fmt.Errorf("gateway: load tls keypair: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	g.log.Info("runner listener starting", "addr", srv.Addr, "tls", srv.TLSConfig != nil)
	var err error
	if srv.TLSConfig != nil {
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

const helloTimeout = 15 * time.Second

func (g *Gateway) handleRunner(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Runners are daemons, not browsers; there is no origin to honour.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		g.log.Warn("websocket accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	// Blob transfers are chunked, but a control frame can still be sizeable.
	ws.SetReadLimit(protocol.MaxControlFrame + protocol.MaxChunk + 4096)

	ctx := r.Context()
	conn, err := g.handshake(ctx, ws, r.RemoteAddr)
	if err != nil {
		g.log.Warn("runner handshake rejected", "remote", r.RemoteAddr, "err", err)
		_ = ws.Close(websocket.StatusPolicyViolation, truncateReason(err.Error()))
		return
	}

	g.serveConn(ctx, conn)
}

// handshake reads the hello frame, negotiates the protocol version, and
// authenticates the runner before anything else is permitted on the socket.
func (g *Gateway) handshake(ctx context.Context, ws *websocket.Conn, remote string) (*Conn, error) {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()

	typ, data, err := ws.Read(hctx)
	if err != nil {
		return nil, fmt.Errorf("reading hello: %w", err)
	}
	if typ != websocket.MessageText {
		return nil, errors.New("first frame must be a text hello frame")
	}
	frame, err := protocol.Decode(data, protocol.DirUp)
	if err != nil {
		return nil, fmt.Errorf("decoding hello: %w", err)
	}
	hello, ok := frame.(*protocol.Hello)
	if !ok {
		return nil, fmt.Errorf("first frame was %s, want hello", frame.Type())
	}

	peerVer, compat, err := protocol.NegotiateString(hello.Protocol)
	if err != nil {
		return nil, err
	}
	if !compat.OK() {
		return nil, fmt.Errorf("protocol %s is incompatible with gateway %s",
			hello.Protocol, protocol.Version)
	}
	if compat == protocol.CompatibleWithWarning {
		g.log.Warn("runner protocol minor skew",
			"runner", hello.Runner, "runner_version", hello.Protocol, "gateway_version", protocol.Version)
	}

	cfg := g.cfg.Load()
	rc, known := cfg.Runners[hello.Runner]
	if !known {
		return nil, fmt.Errorf("runner %q is not in the configuration", hello.Runner)
	}
	if err := g.authenticate(hello, rc); err != nil {
		_ = g.store.Log(store.Event{
			Kind:   "runner.auth_failed",
			Runner: hello.Runner,
			Detail: map[string]any{"remote": remote, "reason": err.Error()},
		})
		return nil, err
	}

	conn := newConn(g, ws, hello, peerVer)

	if old := g.hub.Register(conn); old != nil {
		g.log.Warn("displacing previous connection for runner", "runner", conn.runner)
		old.CloseWith("displaced by a newer connection")
	}

	if err := conn.Send(&protocol.HelloAck{
		Protocol: protocol.Version,
		Runner:   conn.runner,
		Bundle:   protocol.BundleRef{Version: 0},
		Routes:   routesFor(cfg, conn.runner),
		Policy: &protocol.Policy{
			Deny:       rc.Policy.Deny,
			ForgeRepos: rc.Policy.Forge.Repos,
		},
	}); err != nil {
		g.hub.Unregister(conn)
		return nil, err
	}

	_ = g.store.Log(store.Event{
		Kind:   "runner.connected",
		Runner: conn.runner,
		Detail: map[string]any{
			"remote":  remote,
			"harness": hello.Harness,
			"host":    hello.Host,
			"caps":    hello.Capabilities,
		},
	})
	g.log.Info("runner connected", "runner", conn.runner,
		"harness", hello.Harness.Adapter, "host", hello.Host.ID)
	return conn, nil
}

// authenticate verifies the enrollment token in constant time.
func (g *Gateway) authenticate(hello *protocol.Hello, rc *config.Runner) error {
	switch hello.Auth.Mode {
	case protocol.AuthToken:
		name := rc.EffectiveTokenSecret(hello.Runner)
		want, err := g.secrets.Get(name)
		if err != nil {
			return fmt.Errorf("enrollment secret %q is unavailable: %w", name, err)
		}
		if want.Value == "" {
			return fmt.Errorf("enrollment secret %q is empty", name)
		}
		if subtle.ConstantTimeCompare([]byte(want.Value), []byte(hello.Auth.Value)) != 1 {
			return errors.New("enrollment token does not match")
		}
		return nil
	case protocol.AuthInstanceIdentity:
		// Verifying a signed cloud instance identity document removes the shared
		// secret entirely. Until a verifier is wired up, refuse rather than
		// accept an unverified assertion.
		return errors.New("instance-identity auth is not enabled on this gateway")
	default:
		return fmt.Errorf("unsupported auth mode %q", hello.Auth.Mode)
	}
}

// serveConn runs the read, write, and heartbeat loops for a connection.
func (g *Gateway) serveConn(ctx context.Context, conn *Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go conn.writeLoop(ctx)
	go conn.heartbeat(ctx, g.cfg.Load().Gateway.Heartbeat.Duration())

	if err := g.pushBundle(ctx, conn); err != nil {
		g.log.Error("initial bundle push failed", "runner", conn.runner, "err", err)
	}
	g.drainQueue(ctx, conn)

	err := conn.readLoop(ctx)

	conn.CloseWith("read loop ended")
	g.hub.Unregister(conn)
	g.failPendingFor(conn.runner)
	go g.reconcileTurns(conn.runner, 10*g.cfg.Load().Gateway.Heartbeat.Duration())

	status := websocket.CloseStatus(err)
	g.log.Info("runner disconnected", "runner", conn.runner, "close_status", status, "err", err)
	_ = g.store.Log(store.Event{
		Kind:   "runner.disconnected",
		Runner: conn.runner,
		Detail: map[string]any{"close_status": int(status), "error": errString(err)},
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func routesFor(cfg *config.Config, runner string) []string {
	var out []string
	for _, r := range cfg.Routes {
		if r.Runner != runner {
			continue
		}
		if r.DM {
			out = append(out, "<dm>")
			continue
		}
		out = append(out, r.Channel)
	}
	return out
}
