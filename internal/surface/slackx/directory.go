package slackx

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// directory resolves user and channel ids to the names people know them by, so
// the agent can be told who is asking and where. It is an enrichment, never a
// dependency: every failure — a missing scope, a timeout, a rate limit —
// degrades to the bare id, and nothing here can hold up a message for longer
// than one short lookup.
//
// Results are cached. A name changes rarely, and without the cache every
// message would cost an API call against the one rate-limit bucket every
// runner shares. Failures are cached too, for less time: a missing scope would
// otherwise be rediscovered on every message until someone adds it.
type directory struct {
	api     directoryAPI
	ttl     time.Duration
	failTTL time.Duration
	timeout time.Duration
	now     func() time.Time

	mu    sync.Mutex
	users map[string]userEntry
	chans map[string]chanEntry
}

type directoryAPI interface {
	GetUserInfoContext(ctx context.Context, user string) (*slack.User, error)
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
}

type userEntry struct {
	display, email string
	at             time.Time
	failed         bool
}

type chanEntry struct {
	name   string
	at     time.Time
	failed bool
}

const (
	directoryTTL     = time.Hour
	directoryFailTTL = 10 * time.Minute
	directoryTimeout = 2 * time.Second
)

func newDirectory(api directoryAPI) *directory {
	return &directory{
		api: api, ttl: directoryTTL, failTTL: directoryFailTTL, timeout: directoryTimeout,
		now:   time.Now,
		users: map[string]userEntry{},
		chans: map[string]chanEntry{},
	}
}

func (d *directory) fresh(at time.Time, failed bool) bool {
	ttl := d.ttl
	if failed {
		ttl = d.failTTL
	}
	return d.now().Sub(at) < ttl
}

// user returns a display name and email for a user id. Either may be empty:
// the name needs users:read, the email users:read.email.
func (d *directory) user(ctx context.Context, id string) (display, email string) {
	if id == "" {
		return "", ""
	}
	d.mu.Lock()
	e, ok := d.users[id]
	d.mu.Unlock()
	if ok && d.fresh(e.at, e.failed) {
		return e.display, e.email
	}

	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	u, err := d.api.GetUserInfoContext(cctx, id)
	if err != nil || u == nil {
		// Keep a stale name over no name: a transient failure should not make
		// someone anonymous for ten minutes.
		e.at, e.failed = d.now(), true
		d.mu.Lock()
		d.users[id] = e
		d.mu.Unlock()
		return e.display, e.email
	}
	e = userEntry{display: userDisplay(u), email: u.Profile.Email, at: d.now()}
	d.mu.Lock()
	d.users[id] = e
	d.mu.Unlock()
	return e.display, e.email
}

// userDisplay is the name Slack shows: the display name if set, else the real
// name, else the handle.
func userDisplay(u *slack.User) string {
	for _, n := range []string{u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return ""
}

// channel returns a channel's name (needs channels:read, or groups:read for a
// private channel).
func (d *directory) channel(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	d.mu.Lock()
	e, ok := d.chans[id]
	d.mu.Unlock()
	if ok && d.fresh(e.at, e.failed) {
		return e.name
	}

	cctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	ch, err := d.api.GetConversationInfoContext(cctx, &slack.GetConversationInfoInput{ChannelID: id})
	if err != nil || ch == nil {
		e.at, e.failed = d.now(), true
		d.mu.Lock()
		d.chans[id] = e
		d.mu.Unlock()
		return e.name
	}
	d.rememberChannel(id, ch.Name)
	return ch.Name
}

// rememberChannel records a name learned elsewhere (the membership check reads
// the same API), so it costs no second call.
func (d *directory) rememberChannel(id, name string) {
	if name == "" {
		return
	}
	d.mu.Lock()
	d.chans[id] = chanEntry{name: name, at: d.now()}
	d.mu.Unlock()
}
