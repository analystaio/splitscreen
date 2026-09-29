package slackx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/slack-go/slack"

	"github.com/avarant/splitscreen/internal/surface"
)

func statusServer(t *testing.T, slackErr string, got *url.Values) *Surface {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/assistant.threads.setStatus" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		*got = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if slackErr == "" {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, slackErr)
	}))
	t.Cleanup(srv.Close)
	return &Surface{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))}
}

func TestSetStatusSendsThreadAndPersona(t *testing.T) {
	var got url.Values
	s := statusServer(t, "", &got)
	err := s.SetStatus(context.Background(), surface.Status{
		Channel: "C1", Thread: "1700.1", Text: "is working…",
		Persona: surface.Persona{Name: "Ada", Icon: ":atom_symbol:"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"channel_id": "C1", "thread_ts": "1700.1", "status": "is working…",
		"username": "Ada", "icon_emoji": ":atom_symbol:",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

// Permanent failures are marked so the gateway latches the channel off;
// transient ones are not.
func TestSetStatusClassifiesErrors(t *testing.T) {
	for code, permanent := range map[string]bool{
		"missing_scope": true, "not_allowed_token_type": true, "channel_not_found": true,
		"ratelimited": false, "internal_error": false,
	} {
		var got url.Values
		s := statusServer(t, code, &got)
		err := s.SetStatus(context.Background(), surface.Status{Channel: "C1", Thread: "1", Text: "x"})
		if err == nil {
			t.Fatalf("%s: expected an error", code)
		}
		if errors.Is(err, surface.ErrStatusUnavailable) != permanent {
			t.Errorf("%s: permanent = %v, want %v (%v)", code, !permanent, permanent, err)
		}
	}
}
