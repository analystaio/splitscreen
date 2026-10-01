package gateway

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/analystaio/splitscreen/internal/store"
	"github.com/analystaio/splitscreen/internal/surface"
)

// The web view is deliberately small: server-rendered HTML embedded in the
// binary, no build step, no client framework. Slash commands already cover most
// of what an operator asks day to day, and building a single-page app up front
// is the easiest way to spend a month on the least valuable part of this
// system.
//
// It binds to loopback by default, so access is via a port-forward — which
// makes authorization the platform's problem (and therefore already solved)
// rather than something this process has to invent.

// ServeWeb starts the read-only status page and blocks until ctx is cancelled.
func (g *Gateway) ServeWeb(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.handleIndex)
	mux.HandleFunc("/audit", g.handleAudit)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	g.log.Info("web view starting", "addr", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type webRunner struct {
	Name     string
	Persona  string
	State    string
	Online   bool
	Since    string
	Bundle   int
	Harness  string
	Cwd      string
	Idle     string
	Queued   int
	Channels []webChannel
	Billing  string
}

type webChannel struct {
	Label   string
	Problem string
}

type webData struct {
	Runners   []webRunner
	Proxied   []string
	Forge     string
	Costs     []store.CostRow
	Threads   []store.CostRow
	PriceVer  string
	Generated string
}

func (g *Gateway) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cfg := g.cfg.Load()
	heartbeat := cfg.Gateway.Heartbeat.Duration()

	names := make([]string, 0, len(cfg.Runners))
	for n := range cfg.Runners {
		names = append(names, n)
	}
	sort.Strings(names)

	routes := map[string][]webChannel{}
	for _, rt := range cfg.Routes {
		if rt.DM {
			routes[rt.Runner] = append(routes[rt.Runner], webChannel{Label: "direct messages"})
			continue
		}
		wc := webChannel{Label: rt.Channel}
		if st, ok := g.ChannelState(rt.Channel); ok {
			if st.Info.Name != "" {
				wc.Label = "#" + st.Info.Name
			}
			switch st.Info.Membership {
			case surface.MembershipNotJoined:
				wc.Problem = "not joined — " + st.Info.Detail
			case surface.MembershipUnknown:
				wc.Problem = "unverified"
			}
		}
		routes[rt.Runner] = append(routes[rt.Runner], wc)
	}

	data := webData{
		Forge:     "not configured",
		PriceVer:  g.prices.Version,
		Generated: time.Now().Format(time.RFC3339),
	}
	if g.forge != nil {
		data.Forge = g.forge.Name()
	}
	data.Proxied = g.proxy.Names()
	sort.Strings(data.Proxied)

	for _, name := range names {
		rc := cfg.Runners[name]
		depth, _ := g.store.QueueDepth(name)
		row := webRunner{
			Name:     name,
			Persona:  rc.Display.Name,
			Cwd:      rc.Cwd,
			Idle:     rc.Idle.String(),
			Harness:  rc.Harness,
			Billing:  rc.Billing,
			Queued:   depth,
			Channels: routes[name],
			State:    string(StateDisconnected),
		}
		if conn, ok := g.hub.Get(name); ok {
			row.Online = true
			row.State = string(conn.State(heartbeat))
			row.Since = humanDuration(time.Since(conn.ConnectedAt()))
			row.Bundle = conn.BundleVersion()
			if h := conn.Harness(); h.Adapter != "" {
				row.Harness = h.Adapter
			}
		}
		data.Runners = append(data.Runners, row)
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	data.Costs, _ = g.store.CostByRunner(since)
	data.Threads, _ = g.store.TopThreads(since, 10)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		g.log.Error("render status page failed", "err", err)
	}
}

func (g *Gateway) handleAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "The audit log lives in the gateway's SQLite store.")
	fmt.Fprintln(w, "Query it directly; it is deliberately not exposed over HTTP,")
	fmt.Fprintln(w, "because it contains message text, tool inputs, and decisions.")
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Splitscreen</title>
<style>
  :root { color-scheme: light dark; --fg:#111; --muted:#666; --line:#ddd; --bg:#fff; }
  @media (prefers-color-scheme: dark) {
    :root { --fg:#e6e6e6; --muted:#9a9a9a; --line:#333; --bg:#141414; }
  }
  body { font: 15px/1.5 ui-sans-serif, system-ui, sans-serif; margin: 0; padding: 2rem;
         color: var(--fg); background: var(--bg); max-width: 68rem; }
  h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
  h2 { font-size: 1rem; margin: 2rem 0 .5rem; text-transform: uppercase;
       letter-spacing: .06em; color: var(--muted); }
  .sub { color: var(--muted); margin: 0 0 1.5rem; font-size: .875rem; }
  table { border-collapse: collapse; width: 100%; font-size: .9rem; }
  th, td { text-align: left; padding: .5rem .6rem; border-bottom: 1px solid var(--line);
           vertical-align: top; }
  th { font-weight: 600; color: var(--muted); font-size: .8rem;
       text-transform: uppercase; letter-spacing: .04em; }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  code { font: .85em ui-monospace, monospace; }
  .dot { display:inline-block; width:.6rem; height:.6rem; border-radius:50%;
         margin-right:.4rem; vertical-align: baseline; }
  .connected { background:#1a9d4b } .degraded { background:#c98a00 }
  .disconnected { background:#c23b22 }
  .warn { color:#c98a00 }
  .wrap { overflow-x: auto; }
</style>
</head>
<body>
<h1>Splitscreen</h1>
<p class="sub">Generated {{.Generated}} · forge: {{.Forge}}
{{if .Proxied}}· proxied MCP: {{range $i, $s := .Proxied}}{{if $i}}, {{end}}<code>{{$s}}</code>{{end}}{{end}}</p>

<h2>Runners</h2>
<div class="wrap">
<table>
  <tr><th>Name</th><th>State</th><th>Uptime</th><th>Bundle</th><th>Harness</th>
      <th>Working tree</th><th>Idle</th><th>Routes</th><th class="num">Queued</th></tr>
  {{range .Runners}}
  <tr>
    <td><code>{{.Name}}</code>{{if .Persona}}<br><span class="sub">posts as {{.Persona}}</span>{{end}}</td>
    <td><span class="dot {{.State}}"></span>{{.State}}</td>
    <td>{{if .Online}}{{.Since}}{{else}}—{{end}}</td>
    <td>{{if .Online}}v{{.Bundle}}{{else}}—{{end}}</td>
    <td>{{.Harness}}{{if .Billing}}<br><span class="sub">{{.Billing}}</span>{{end}}</td>
    <td><code>{{.Cwd}}</code></td>
    <td>{{.Idle}}</td>
    <td>{{if .Channels}}{{range $i, $c := .Channels}}{{if $i}}<br>{{end}}<code>{{$c.Label}}</code>
          {{if $c.Problem}}<br><span class="warn">{{$c.Problem}}</span>{{end}}{{end}}
        {{else}}<span class="warn">no routes</span>{{end}}</td>
    <td class="num">{{if .Queued}}<span class="warn">{{.Queued}}</span>{{else}}0{{end}}</td>
  </tr>
  {{end}}
</table>
</div>

<h2>Usage — last 7 days</h2>
<div class="wrap">
<table>
  <tr><th>Runner</th><th class="num">Turns</th><th class="num">Input</th>
      <th class="num">Cache write</th><th class="num">Cache read</th>
      <th class="num">Output</th><th class="num">Cost</th><th class="num">Unmetered</th></tr>
  {{range .Costs}}
  <tr>
    <td><code>{{.Key}}</code></td>
    <td class="num">{{.Turns}}</td>
    <td class="num">{{.Input}}</td>
    <td class="num">{{.CacheWrite}}</td>
    <td class="num">{{.CacheRead}}</td>
    <td class="num">{{.Output}}</td>
    <td class="num">${{printf "%.2f" .CostUSD}}</td>
    <td class="num">{{if .UnknownTurns}}<span class="warn">{{.UnknownTurns}}</span>{{else}}0{{end}}</td>
  </tr>
  {{end}}
</table>
</div>
<p class="sub">Cache reads bill at a fraction of input and cache writes at a premium,
so the counters are never summed. Unmetered turns reported no usage and are
excluded from every cost above — they are not free, only unmeasured.
Priced with table <code>{{.PriceVer}}</code>.</p>

{{if .Threads}}
<h2>Most expensive threads</h2>
<div class="wrap">
<table>
  <tr><th>Thread</th><th class="num">Turns</th><th class="num">Cost</th></tr>
  {{range .Threads}}
  <tr><td><code>{{.Key}}</code></td><td class="num">{{.Turns}}</td>
      <td class="num">${{printf "%.2f" .CostUSD}}</td></tr>
  {{end}}
</table>
</div>
{{end}}
</body>
</html>
`))
