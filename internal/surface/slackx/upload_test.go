package slackx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/analystaio/splitscreen/internal/surface"
)

// uploadServer stands in for Slack's three-step external upload flow and
// captures the form files.completeUploadExternal is called with, which is where
// a file's accompanying message is set.
func uploadServer(t *testing.T, complete *url.Values) *Surface {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/files.getUploadURLExternal":
			fmt.Fprintf(w, `{"ok":true,"upload_url":%q,"file_id":"F1"}`, srv.URL+"/put")
		case "/put":
			fmt.Fprint(w, `{"ok":true}`)
		case "/files.completeUploadExternal":
			_ = r.ParseForm()
			*complete = r.PostForm
			fmt.Fprint(w, `{"ok":true,"files":[{"id":"F1"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Surface{api: slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))}
}

// A file and the sentence explaining it should land as one Slack message, which
// is what initial_comment does. Before this the comment never left the runner.
func TestUploadPostsCommentWithTheFile(t *testing.T) {
	var complete url.Values
	s := uploadServer(t, &complete)

	err := s.Upload(context.Background(), surface.Upload{
		Channel: "C1", Thread: "1700.1", Name: "chart.png", Mime: "image/png",
		Comment: "Revenue by **quarter**", Content: strings.NewReader("png"), Size: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Converted to mrkdwn: Slack renders **bold** literally.
	if got := complete.Get("initial_comment"); got != "Revenue by *quarter*" {
		t.Errorf("initial_comment = %q", got)
	}
	if got := complete.Get("thread_ts"); got != "1700.1" {
		t.Errorf("thread_ts = %q", got)
	}
}

// No comment must not post an empty one: Slack rejects a blank initial_comment,
// and a bare file is the long-standing behaviour.
func TestUploadWithoutCommentSendsNone(t *testing.T) {
	var complete url.Values
	s := uploadServer(t, &complete)

	err := s.Upload(context.Background(), surface.Upload{
		Channel: "C1", Name: "chart.png", Content: strings.NewReader("png"), Size: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := complete["initial_comment"]; ok {
		t.Errorf("initial_comment was sent: %q", complete.Get("initial_comment"))
	}
}
