package slackx

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type fakeDirAPI struct {
	mu        sync.Mutex
	userCalls int
	chanCalls int
	user      *slack.User
	userErr   error
	chanName  string
	chanErr   error
}

func (f *fakeDirAPI) GetUserInfoContext(context.Context, string) (*slack.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls++
	return f.user, f.userErr
}

func (f *fakeDirAPI) GetConversationInfoContext(context.Context, *slack.GetConversationInfoInput) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chanCalls++
	if f.chanErr != nil {
		return nil, f.chanErr
	}
	ch := &slack.Channel{}
	ch.Name = f.chanName
	return ch, nil
}

func testDirectory(api directoryAPI) (*directory, *time.Time) {
	d := newDirectory(api)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	return d, &now
}

func TestDirectoryResolvesAndCaches(t *testing.T) {
	u := &slack.User{Name: "anna"}
	u.Profile.RealName = "Anna Petrosyan"
	u.Profile.Email = "anna@example.com"
	api := &fakeDirAPI{user: u, chanName: "box-foo"}
	d, now := testDirectory(api)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		name, email := d.user(ctx, "U1")
		if name != "Anna Petrosyan" || email != "anna@example.com" {
			t.Fatalf("user = %q %q", name, email)
		}
		if got := d.channel(ctx, "C1"); got != "box-foo" {
			t.Fatalf("channel = %q", got)
		}
	}
	if api.userCalls != 1 || api.chanCalls != 1 {
		t.Fatalf("calls user=%d chan=%d; lookups should be cached", api.userCalls, api.chanCalls)
	}

	*now = now.Add(directoryTTL + time.Second)
	d.user(ctx, "U1")
	if api.userCalls != 2 {
		t.Error("an expired entry was not refreshed")
	}
}

// A missing scope must degrade to ids, be remembered (not retried on every
// message), and never cost a name that was already known.
func TestDirectoryDegradesOnFailure(t *testing.T) {
	api := &fakeDirAPI{userErr: errors.New("missing_scope"), chanErr: errors.New("missing_scope")}
	d, now := testDirectory(api)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if name, email := d.user(ctx, "U1"); name != "" || email != "" {
			t.Fatalf("got %q %q from a failing lookup", name, email)
		}
		if d.channel(ctx, "C1") != "" {
			t.Fatal("got a channel name from a failing lookup")
		}
	}
	if api.userCalls != 1 || api.chanCalls != 1 {
		t.Fatalf("failures retried on every message: user=%d chan=%d", api.userCalls, api.chanCalls)
	}
	*now = now.Add(directoryFailTTL + time.Second)
	d.user(ctx, "U1")
	if api.userCalls != 2 {
		t.Error("a failure was cached for longer than the failure TTL")
	}

	// Known name, then the API starts failing: keep the stale name.
	d.rememberChannel("C2", "known")
	*now = now.Add(directoryTTL + time.Second)
	if got := d.channel(ctx, "C2"); got != "known" {
		t.Errorf("channel = %q; a transient failure should keep the stale name", got)
	}
}

func TestUserDisplayPreference(t *testing.T) {
	u := &slack.User{Name: "handle", RealName: "Top Real"}
	if got := userDisplay(u); got != "Top Real" {
		t.Errorf("got %q", got)
	}
	u.Profile.RealName = "Profile Real"
	if got := userDisplay(u); got != "Profile Real" {
		t.Errorf("got %q", got)
	}
	u.Profile.DisplayName = "Anna"
	if got := userDisplay(u); got != "Anna" {
		t.Errorf("got %q", got)
	}
}
