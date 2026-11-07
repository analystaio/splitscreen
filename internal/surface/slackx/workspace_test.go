package slackx

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/slack-go/slack"
)

// fakeSlack serves Slack Web API methods by name.
func fakeSlack(t *testing.T, routes map[string]func(url string, form map[string]string) any) *Surface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.Form {
			form[k] = r.Form.Get(k)
		}
		method := strings.TrimPrefix(r.URL.Path, "/")
		h, ok := routes[method]
		if !ok {
			t.Errorf("unexpected call to %s", method)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unexpected"})
			return
		}
		_ = json.NewEncoder(w).Encode(h(method, form))
	}))
	t.Cleanup(srv.Close)
	return &Surface{api: slack.New("x", slack.OptionAPIURL(srv.URL+"/"))}
}

// The bypass this exists for: the raw text has no <! in it, and rendering adds one.
func TestPostToChannelChecksTheRenderedText(t *testing.T) {
	var calls atomic.Int32
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"chat.postMessage": func(string, map[string]string) any {
			calls.Add(1)
			return map[string]any{"ok": true, "channel": "C1", "ts": "1.1"}
		},
	})
	ctx := context.Background()
	for _, text := range []string{"[x](!channel)", "[a](!here)", "<!everyone>", "ping [t](!subteam^S1)"} {
		if _, err := s.PostToChannel(ctx, "C1", text, surface.Persona{}); !errors.Is(err, surface.ErrBroadcast) {
			t.Errorf("%q: err = %v", text, err)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d broadcasts reached Slack", n)
	}
	if _, err := s.PostToChannel(ctx, "C1", "hello <@U1>", surface.Persona{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("an ordinary post did not reach Slack")
	}
}

func TestPostToChannelSendsRenderedText(t *testing.T) {
	var got string
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"chat.postMessage": func(_ string, f map[string]string) any {
			got = f["text"]
			return map[string]any{"ok": true, "channel": "C1", "ts": "1.1"}
		},
	})
	if _, err := s.PostToChannel(context.Background(), "C1", "**bold**", surface.Persona{}); err != nil {
		t.Fatal(err)
	}
	if got != "*bold*" {
		t.Fatalf("text = %q", got)
	}
}

func TestIsChannelMemberPages(t *testing.T) {
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"conversations.members": func(_ string, f map[string]string) any {
			if f["cursor"] == "" {
				return map[string]any{"ok": true, "members": []string{"U1"},
					"response_metadata": map[string]string{"next_cursor": "two"}}
			}
			return map[string]any{"ok": true, "members": []string{"U2"}}
		},
	})
	ctx := context.Background()
	if ok, err := s.IsChannelMember(ctx, "C1", "U2"); err != nil || !ok {
		t.Fatalf("second page: %v %v", ok, err)
	}
	if ok, err := s.IsChannelMember(ctx, "C1", "U3"); err != nil || ok {
		t.Fatalf("nobody: %v %v", ok, err)
	}
}

func TestJoinedChannelsPages(t *testing.T) {
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"users.conversations": func(_ string, f map[string]string) any {
			if f["cursor"] == "" {
				return map[string]any{"ok": true, "channels": []map[string]string{{"id": "C1", "name": "a"}},
					"response_metadata": map[string]string{"next_cursor": "two"}}
			}
			return map[string]any{"ok": true, "channels": []map[string]string{{"id": "C2", "name": "b"}}}
		},
	})
	got, err := s.JoinedChannels(context.Background())
	if err != nil || len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestHistoryIsOldestFirst(t *testing.T) {
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"conversations.history": func(string, map[string]string) any {
			return map[string]any{"ok": true, "messages": []map[string]any{
				{"ts": "3.0", "text": "newest", "user": "U1"},
				{"ts": "1.0", "text": "oldest", "user": "U1", "reply_count": 2},
			}}
		},
	})
	got, err := s.ChannelHistory(context.Background(), surface.HistoryRequest{Channel: "C1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "oldest" || got[0].Replies != 2 {
		t.Fatalf("got %+v", got)
	}
}

// conversations.replies comes oldest first, so the limit must keep the tail.
func TestThreadKeepsTheNewestReplies(t *testing.T) {
	s := fakeSlack(t, map[string]func(string, map[string]string) any{
		"conversations.replies": func(string, map[string]string) any {
			return map[string]any{"ok": true, "messages": []map[string]any{
				{"ts": "1.0", "text": "one"}, {"ts": "2.0", "text": "two"}, {"ts": "3.0", "text": "three"},
			}}
		},
	})
	got, err := s.ChannelHistory(context.Background(), surface.HistoryRequest{Channel: "C1", Thread: "1.0", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "two" || got[1].Text != "three" {
		t.Fatalf("got %+v", got)
	}
}
