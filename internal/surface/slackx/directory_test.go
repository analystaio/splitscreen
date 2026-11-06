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
	u := &slack.User{Name: "jane"}
	u.Profile.RealName = "Jane Doe"
	u.Profile.Email = "jane@example.com"
	api := &fakeDirAPI{user: u, chanName: "box-foo"}
	d, now := testDirectory(api)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		name, email := d.user(ctx, "U1")
		if name != "Jane Doe" || email != "jane@example.com" {
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
	u.Profile.DisplayName = "Jane"
	if got := userDisplay(u); got != "Jane" {
		t.Errorf("got %q", got)
	}
}

func TestFullMember(t *testing.T) {
	base := func() *slack.User { return &slack.User{TeamID: "T1"} }
	cases := []struct {
		name string
		mut  func(*slack.User)
		team string
		want bool
	}{
		{"member", func(*slack.User) {}, "T1", true},
		{"multi-channel guest", func(u *slack.User) { u.IsRestricted = true }, "T1", false},
		{"single-channel guest", func(u *slack.User) { u.IsUltraRestricted = true }, "T1", false},
		{"stranger", func(u *slack.User) { u.IsStranger = true }, "T1", false},
		{"deactivated", func(u *slack.User) { u.Deleted = true }, "T1", false},
		{"bot", func(u *slack.User) { u.IsBot = true }, "T1", false},
		{"other team, no flags", func(u *slack.User) { u.TeamID = "T2" }, "T1", false},
		{"own team unknown", func(*slack.User) {}, "", false},
		{"both teams empty", func(u *slack.User) { u.TeamID = "" }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := base()
			tc.mut(u)
			if got := fullMember(u, tc.team); got != tc.want {
				t.Fatalf("fullMember = %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeChanAPI struct {
	fakeDirAPI
	ch *slack.Channel
}

func (f *fakeChanAPI) GetConversationInfoContext(context.Context, *slack.GetConversationInfoInput) (*slack.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chanCalls++
	return f.ch, f.chanErr
}

func TestIsInternal(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*slack.Channel)
		want bool
	}{
		{"internal", func(*slack.Channel) {}, true},
		{"shared", func(c *slack.Channel) { c.IsShared = true }, false},
		{"slack connect", func(c *slack.Channel) { c.IsExtShared = true }, false},
		{"org shared", func(c *slack.Channel) { c.IsOrgShared = true }, false},
		{"invite pending", func(c *slack.Channel) { c.IsPendingExtShared = true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := &slack.Channel{}
			tc.mut(ch)
			d, _ := testDirectory(&fakeChanAPI{ch: ch})
			if got := d.isInternal(context.Background(), "C1"); got != tc.want {
				t.Fatalf("isInternal = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTrustFailsClosedOnLookupError(t *testing.T) {
	d, _ := testDirectory(&fakeDirAPI{userErr: errors.New("ratelimited")})
	if d.isMember(context.Background(), "U1", "T1") {
		t.Fatal("a failed user lookup counted as a member")
	}
	c, _ := testDirectory(&fakeChanAPI{fakeDirAPI: fakeDirAPI{chanErr: errors.New("missing_scope")}})
	if c.isInternal(context.Background(), "C1") {
		t.Fatal("a failed channel lookup counted as internal")
	}
}

func TestTrustTreatsAnEmptyAnswerAsFailure(t *testing.T) {
	d, _ := testDirectory(&fakeDirAPI{})
	if d.isMember(context.Background(), "U1", "T1") {
		t.Fatal("a nil user counted as a member")
	}
}

// The property that makes this different from the name cache, which keeps a stale
// value through a failure on purpose.
func TestAnExpiredYesDoesNotSurviveAFailedLookup(t *testing.T) {
	api := &fakeDirAPI{user: &slack.User{TeamID: "T1"}}
	d, now := testDirectory(api)
	ctx := context.Background()
	if !d.isMember(ctx, "U1", "T1") {
		t.Fatal("want member")
	}
	api.mu.Lock()
	api.user, api.userErr = nil, errors.New("timeout")
	api.mu.Unlock()
	*now = now.Add(trustTTL + time.Second)
	if d.isMember(ctx, "U1", "T1") {
		t.Fatal("an expired yes survived a failed lookup")
	}
}

func TestTrustIsCachedOnlyBriefly(t *testing.T) {
	api := &fakeDirAPI{user: &slack.User{TeamID: "T1"}}
	d, now := testDirectory(api)
	ctx := context.Background()
	d.isMember(ctx, "U1", "T1")
	d.isMember(ctx, "U1", "T1")
	if api.userCalls != 1 {
		t.Fatalf("calls = %d, want 1 within the TTL", api.userCalls)
	}

	// Demoted to a guest: within trustTTL the old answer stands, and after it not.
	api.mu.Lock()
	api.user = &slack.User{TeamID: "T1", IsRestricted: true}
	api.mu.Unlock()
	*now = now.Add(trustTTL + time.Second)
	if d.isMember(ctx, "U1", "T1") {
		t.Fatal("a demoted user stayed a member past the TTL")
	}
}

func TestEmptyIDIsNeverTrusted(t *testing.T) {
	d, _ := testDirectory(&fakeDirAPI{user: &slack.User{TeamID: "T1"}})
	if d.isMember(context.Background(), "", "T1") {
		t.Fatal("an empty id counted as a member")
	}
}
