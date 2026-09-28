// Package gateway is the control plane: it owns the chat surfaces, every
// credential, the routing table, and the audit log. Runners own working trees
// and execute agents; they hold nothing secret at rest.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avarant/splitscreen/config"
	"github.com/avarant/splitscreen/internal/forge"
	"github.com/avarant/splitscreen/internal/mcpproxy"
	"github.com/avarant/splitscreen/internal/pricing"
	"github.com/avarant/splitscreen/internal/secrets"
	"github.com/avarant/splitscreen/internal/store"
	"github.com/avarant/splitscreen/internal/surface"
	"github.com/avarant/splitscreen/internal/wake"
)

// Gateway is the singleton control plane.
//
// Exactly one may run against a given chat app: two connections to one app
// means the platform delivers each payload to one of them at random, which is
// the delivery bug this whole architecture exists to eliminate.
type Gateway struct {
	cfgPath string
	cfg     atomic.Pointer[config.Config]

	store   *store.Store
	secrets secrets.Backend
	prices  *pricing.Table
	proxy   *mcpproxy.Proxy
	forge   forge.Provider
	log     *slog.Logger

	surfaces map[string]surface.Surface
	hub      *Hub

	streams sync.Map // turn id -> *stream
	// streamWarned keeps the "this surface cannot stream" notice to one line
	// per process rather than one per turn.
	streamWarned atomic.Bool
	prompts      sync.Map // permission request id -> *pendingPrompt
	turns        sync.Map // turn id -> *turnContext

	bundlesMu sync.Mutex
	bundles   map[string]bundleState // runner -> last pushed bundle

	queuesMu sync.Mutex
	queues   map[string]*runnerQueue // runner -> concurrency queue
	// Per-thread serialization: at most one in-flight turn per thread. Guarded
	// by queuesMu (same lock as the runner queues — they interact on dispatch).
	threadActive  map[string]bool          // thread key -> a turn is in flight
	threadWaiting map[string][]*queuedTurn // thread key -> messages parked behind it

	channels channelCache
	grants   *grantStore

	// heldFiles keeps attachments for messages queued while their runner is
	// offline (turn id -> []surface.File), relayed when the queue drains. In
	// memory only: a file handle is a closure over the surface credential, so
	// a gateway restart before the runner returns drops them.
	heldFiles sync.Map

	waker          wake.Starter
	wakeMu         sync.Mutex
	wakes          map[string]*wakeState
	wakeRetryEvery time.Duration
	wakeRetryFor   time.Duration
}

// runnerQueue bounds how many turns a runner runs at once. active is the count
// currently dispatched (each decremented exactly once when its turn ends);
// waiting holds turns admitted but not yet started, in arrival order.
type runnerQueue struct {
	active  int
	waiting []*queuedTurn
}

// queuedTurn is an inbound message parked because its runner is at capacity.
// It carries everything dispatchTurn needs plus the ref of its in-thread
// position notice, which is edited as the queue advances.
type queuedTurn struct {
	in        surface.Inbound
	runner    string
	rc        *config.Runner
	persona   surface.Persona
	noticeRef surface.Ref
}

// maxQueueDepth bounds a runner's waiting list. A team that has queued this
// many is better told to come back than to pile on unbounded latency.
const maxQueueDepth = 25

// Options configures a gateway.
type Options struct {
	ConfigPath string
	Config     *config.Config
	Store      *store.Store
	Secrets    secrets.Backend
	Prices     *pricing.Table
	Forge      forge.Provider
	Surfaces   map[string]surface.Surface
	Logger     *slog.Logger
	// Waker starts the machine of an offline runner that declares wake. Nil
	// means wake requests are reported in-thread as impossible.
	Waker wake.Starter
}

// New builds a gateway. It does not connect anything; call Run.
func New(o Options) (*Gateway, error) {
	if o.Config == nil {
		return nil, errors.New("gateway: config is required")
	}
	if o.Store == nil {
		return nil, errors.New("gateway: store is required")
	}
	if o.Secrets == nil {
		o.Secrets = secrets.NewEnvBackend()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	if o.Prices == nil {
		o.Prices = DefaultPriceTable()
	}
	if err := o.Prices.Validate(); err != nil {
		return nil, err
	}

	g := &Gateway{
		cfgPath:       o.ConfigPath,
		store:         o.Store,
		secrets:       o.Secrets,
		prices:        o.Prices,
		proxy:         mcpproxy.New(),
		forge:         o.Forge,
		log:           o.Logger,
		surfaces:      o.Surfaces,
		hub:           NewHub(),
		bundles:       map[string]bundleState{},
		queues:        map[string]*runnerQueue{},
		threadActive:  map[string]bool{},
		threadWaiting: map[string][]*queuedTurn{},
		grants:        newGrantStore(),
		waker:         o.Waker,
		wakes:         map[string]*wakeState{},

		wakeRetryEvery: defaultWakeRetryEvery,
		wakeRetryFor:   defaultWakeRetryFor,
	}
	g.channels.byID = map[string]channelState{}
	if g.surfaces == nil {
		g.surfaces = map[string]surface.Surface{}
	}
	g.applyConfig(o.Config)
	for _, w := range o.Config.Warnings {
		g.log.Warn("config warning", "detail", w)
	}
	return g, nil
}

// Config returns the live configuration.
func (g *Gateway) Config() *config.Config { return g.cfg.Load() }

// applyConfig swaps in a validated config and reconfigures derived state.
func (g *Gateway) applyConfig(c *config.Config) {
	g.cfg.Store(c)

	var servers []mcpproxy.Server
	for name, s := range c.MCP {
		if s == nil || s.Kind != config.MCPProxied {
			continue
		}
		srv := mcpproxy.Server{
			Name: name,
			URL:  s.URL,
			Auth: mcpproxy.AuthKind(s.Auth),
			User: s.User,
			Deny: s.Deny,
		}
		if s.Secret != "" {
			sec, err := g.secrets.Get(s.Secret)
			if err != nil {
				// A proxied server whose credential is missing is registered
				// without one so calls fail loudly at the boundary rather than
				// the server silently disappearing from the roster.
				g.log.Error("mcp secret unavailable", "server", name, "secret", s.Secret, "err", err)
			} else {
				srv.Secret = sec.Value
			}
		}
		servers = append(servers, srv)
	}
	g.proxy.Configure(servers)
}

// Reload re-reads the config file. On any validation problem the running config
// is left untouched: a bad edit must never partially apply.
func (g *Gateway) Reload() error {
	if g.cfgPath == "" {
		return errors.New("gateway: no config path to reload from")
	}
	c, err := config.Load(g.cfgPath)
	if err != nil {
		return err
	}
	old := g.cfg.Load()
	g.invalidateRunnerSecrets(old, c)
	g.applyConfig(c)
	g.log.Info("config reloaded", "runners", len(c.Runners), "routes", len(c.Routes))
	for _, w := range c.Warnings {
		g.log.Warn("config warning", "detail", w)
	}

	// Re-check membership: a reload is the most likely moment for a route to
	// have been added to a channel nobody invited the bot into.
	go g.RefreshChannels(context.Background())

	// Runners whose bundle changed get a push; their live sessions are marked
	// stale so the change is announced rather than discovered.
	for name := range c.Runners {
		if conn, ok := g.hub.Get(name); ok {
			if err := g.pushBundle(context.Background(), conn); err != nil {
				g.log.Error("bundle push after reload failed", "runner", name, "err", err)
			}
		}
	}
	if old != nil {
		for name := range old.Runners {
			if _, still := c.Runners[name]; !still {
				if conn, ok := g.hub.Get(name); ok {
					g.log.Warn("runner removed from config; closing", "runner", name)
					conn.CloseWith("runner removed from configuration")
				}
				g.dropRemovedRunner(name)
			}
		}
	}
	return nil
}

// dropRemovedRunner discards what was waiting for a runner that no longer
// exists. Its queued messages can never be delivered, and their turns — which
// are exempt from the stranded sweep because they are waiting, not stuck —
// would otherwise hold their threads forever.
func (g *Gateway) dropRemovedRunner(name string) {
	ctx := context.Background()
	if n, err := g.store.PurgeQueue(name); err != nil {
		g.log.Error("purging a removed runner's queue failed", "runner", name, "err", err)
	} else if n > 0 {
		g.log.Warn("discarded messages queued for a removed runner", "runner", name, "count", n)
	}
	g.turns.Range(func(_, value any) bool {
		turn := value.(*turnContext)
		if turn.Runner == name && turn.queued.Load() {
			g.abandonTurn(ctx, turn, "runner_removed: queued for a runner that was removed")
			if srf, ok := g.surfaceFor(turn.Surface); ok {
				_, _ = srf.Post(ctx, surface.Post{
					Channel: turn.Channel, Thread: turn.Thread, Persona: turn.Persona,
					Text: fmt.Sprintf("`%s` was removed before it came back; your queued message was discarded.", name),
				})
			}
		}
		return true
	})
	g.wakeMu.Lock()
	delete(g.wakes, name)
	g.wakeMu.Unlock()
}

// invalidateRunnerSecrets drops cached enrollment secrets for every runner a
// reload adds, removes, or re-points. Runners come and go with their machines,
// and a name can be reused: without this, a cached token (or a cached miss,
// from a lookup made before the parameter was written) would outlive the change
// by the cache TTL, and a freshly registered runner would be refused for minutes
// for no visible reason.
func (g *Gateway) invalidateRunnerSecrets(old, next *config.Config) {
	inv, ok := g.secrets.(secrets.Invalidator)
	if !ok {
		return
	}
	names := map[string]bool{}
	secretOf := func(c *config.Config, name string) string {
		if c == nil {
			return ""
		}
		if r, ok := c.Runners[name]; ok && r != nil {
			return r.EffectiveTokenSecret(name)
		}
		return ""
	}
	for name := range next.Runners {
		names[name] = true
	}
	if old != nil {
		for name := range old.Runners {
			names[name] = true
		}
	}
	for name := range names {
		before, after := secretOf(old, name), secretOf(next, name)
		if before == after {
			continue
		}
		for _, s := range []string{before, after} {
			if s != "" {
				inv.Invalidate(s)
			}
		}
	}
}

// Run starts every surface and blocks until ctx is cancelled. The runner-facing
// listener is started separately by ServeRunners.
func (g *Gateway) Run(ctx context.Context) error {
	// A restart empties the in-memory turn map, and events for unknown turns
	// are dropped, so any row still "running" now can never be finished by its
	// Done frame. Close them out rather than leaving permanent zombies.
	if n, err := g.store.FailOpenTurns("gateway_restart: turn was in flight when the gateway restarted"); err != nil {
		g.log.Error("failing open turns at startup failed", "err", err)
	} else if n > 0 {
		g.log.Info("closed out turns left running by a previous gateway", "count", n)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(g.surfaces)+1)

	for name, s := range g.surfaces {
		wg.Add(1)
		go func(name string, s surface.Surface) {
			defer wg.Done()
			g.log.Info("surface starting", "surface", name)
			if err := s.Start(ctx, g); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("surface %s: %w", name, err)
			}
		}(name, s)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.watchSecretExpiry(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		g.sweepStrandedTurns(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Give the surfaces a moment to authenticate before asking them
		// anything; a check that races startup reports Unknown for no reason.
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		g.watchChannels(ctx)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		wg.Wait()
		return nil
	}
}

// surfaceFor returns the adapter for a surface name.
func (g *Gateway) surfaceFor(name string) (surface.Surface, bool) {
	s, ok := g.surfaces[name]
	return s, ok
}

// runnerConfig looks up a runner definition.
func (g *Gateway) runnerConfig(name string) (*config.Runner, bool) {
	c := g.cfg.Load()
	r, ok := c.Runners[name]
	return r, ok
}

func personaFor(r *config.Runner) surface.Persona {
	if r == nil {
		return surface.Persona{}
	}
	return surface.Persona{Name: r.Display.Name, Icon: r.Display.Icon}
}

// watchSecretExpiry warns ahead of a mandatory credential rotation, turning a
// future outage into a calendar item.
func (g *Gateway) watchSecretExpiry(ctx context.Context) {
	check := func() {
		c := g.cfg.Load()
		window := c.Gateway.SecretExpiryWarning.Duration()
		for _, s := range secrets.Expiring(g.secrets, window, time.Now()) {
			g.log.Warn("secret expiring", "secret", s.Name, "expires_at", s.ExpiresAt)
			_ = g.store.Log(store.Event{
				Kind:   "secret.expiring",
				Detail: map[string]any{"secret": s.Name, "expires_at": s.ExpiresAt},
			})
		}
	}
	check()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// DefaultPriceTable is a starting point, not an authority. Rates change;
// version it and correct it rather than trusting the harness's own figure.
func DefaultPriceTable() *pricing.Table {
	std := pricing.Rates{
		Input: 15, Output: 75,
		CacheWriteMult: 1.25, CacheReadMult: 0.1, CacheWriteMult1h: 2.0,
	}
	return &pricing.Table{
		Version: "unset-defaults",
		Rates: map[string]pricing.Rates{
			"claude-opus":   std,
			"claude-sonnet": {Input: 3, Output: 15, CacheWriteMult: 1.25, CacheReadMult: 0.1, CacheWriteMult1h: 2.0},
			"claude-haiku":  {Input: 1, Output: 5, CacheWriteMult: 1.25, CacheReadMult: 0.1, CacheWriteMult1h: 2.0},
		},
	}
}
