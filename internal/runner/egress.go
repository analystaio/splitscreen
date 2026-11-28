package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/analystaio/splitscreen/protocol"
)

// The egress proxy is how an agent uses a vault secret it cannot read. It
// listens on loopback; the agent calls
//
//	$SPLITSCREEN_EGRESS/s/<secret>/<path>          the secret's first host
//	$SPLITSCREEN_EGRESS/s/<secret>@<host>/<path>   another of its hosts
//
// and the runner forwards the request to the gateway, which attaches the
// secret and makes the real HTTPS call. The runner never holds the secret. It
// adds one fact the gateway cannot know: which thread's agent made the call,
// read from the kernel (attribution.go), and only claimed when the runner is
// hardened (hardened.go).

type egressContextKey struct{}

// startEgress opens the proxy. Failure is logged, not fatal: the runner works
// without it, and secrets simply are not offered to its sessions.
func (r *Runner) startEgress(ctx context.Context) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.log.Warn("egress proxy unavailable", "err", err)
		return
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(r.serveEgress),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      3 * time.Minute,
		// Keep the accepted connection reachable from the handler, for
		// attribution.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, egressContextKey{}, c)
		},
	}
	r.egressURL.Store("http://" + ln.Addr().String())
	r.log.Info("egress proxy listening", "addr", ln.Addr().String(), "hardened", r.hardened)
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.log.Warn("egress proxy stopped", "err", err)
		}
	}()
}

// egressEnv is the SPLITSCREEN_EGRESS value for new sessions, or "" when the
// proxy is down or the gateway is too old to answer it.
func (r *Runner) egressEnv() string {
	if r.gatewayMinor.Load() < protocol.MinorEgress {
		return ""
	}
	u, _ := r.egressURL.Load().(string)
	return u
}

func egressError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Splitscreen-Egress-Error", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// parseEgressPath splits "/s/name@host/rest?q" into its parts.
func parseEgressPath(rawPath, rawQuery string) (secret, host, path string, ok bool) {
	rest, found := strings.CutPrefix(rawPath, "/s/")
	if !found {
		return "", "", "", false
	}
	head, tail, _ := strings.Cut(rest, "/")
	secret, host, _ = strings.Cut(head, "@")
	if secret == "" {
		return "", "", "", false
	}
	path = "/" + tail
	if rawQuery != "" {
		path += "?" + rawQuery
	}
	return secret, host, path, true
}

func (r *Runner) serveEgress(w http.ResponseWriter, req *http.Request) {
	secret, host, path, ok := parseEgressPath(req.URL.EscapedPath(), req.URL.RawQuery)
	if !ok {
		egressError(w, http.StatusNotFound, "usage",
			"call $SPLITSCREEN_EGRESS/s/<secret>/<path>; list_secrets shows what you can use")
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, protocol.MaxEgressBody+1))
	if err != nil {
		egressError(w, http.StatusBadRequest, "body", err.Error())
		return
	}
	if len(body) > protocol.MaxEgressBody {
		egressError(w, http.StatusRequestEntityTooLarge, "too_large", "request bodies are limited to 512 KiB")
		return
	}

	fr := &protocol.EgressRequest{
		RequestID: "egr_" + randomID(),
		Secret:    secret, Host: host, Method: req.Method, Path: path,
		Headers: map[string][]string(req.Header.Clone()), Body: body,
	}
	if c, ok := req.Context().Value(egressContextKey{}).(net.Conn); ok {
		if thread, found := r.attributeConn(c); found {
			fr.ThreadID = thread
			fr.TurnID = r.turnForThread(thread)
			fr.Attributed = r.hardened && fr.TurnID != ""
		}
	}

	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Minute)
	defer cancel()
	v, err := r.request(ctx, fr.RequestID, fr)
	if err != nil {
		egressError(w, http.StatusBadGateway, "gateway", err.Error())
		return
	}
	switch reply := v.(type) {
	case error:
		egressError(w, http.StatusBadGateway, "gateway", reply.Error())
	case *protocol.EgressResponse:
		if reply.Error != nil {
			status := http.StatusForbidden
			if reply.Error.Code == "egress_failed" || reply.Error.Code == "too_large" {
				status = http.StatusBadGateway
			}
			egressError(w, status, reply.Error.Code, reply.Error.Message)
			return
		}
		for k, vs := range reply.Headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(reply.Status)
		_, _ = w.Write(reply.Body)
	default:
		egressError(w, http.StatusBadGateway, "gateway", "unexpected reply")
	}
}
