package runner

import (
	"testing"

	"github.com/analystaio/splitscreen/protocol"
)

func TestWithContext(t *testing.T) {
	cases := []struct {
		ctx, text, want string
	}{
		{"", "hello", "hello"}, // an older gateway sends no header
		{"[Slack #x (C1) · from A (U1)]", "hello", "[Slack #x (C1) · from A (U1)]\nhello"},
		{"[Slack #x (C1) · from A (U1)]", "", "[Slack #x (C1) · from A (U1)]"}, // attachment-only
	}
	for _, c := range cases {
		if got := withContext(&protocol.Message{Context: c.ctx, Text: c.text}); got != c.want {
			t.Errorf("withContext(%q, %q) = %q", c.ctx, c.text, got)
		}
	}
}

// A newer gateway's fields must not break an older decoder's view, and an
// older gateway's frame (no context fields) must decode here.
func TestMessageFieldsAreOptional(t *testing.T) {
	old := []byte(`{"t":"message","thread":"s:C:T","turn":"turn_1","channel":"C","user":{"id":"U"},"text":"hi"}`)
	f, err := protocol.Decode(old, protocol.DirDown)
	if err != nil {
		t.Fatalf("frame from an older gateway: %v", err)
	}
	if m := f.(*protocol.Message); m.Context != "" || withContext(m) != "hi" {
		t.Errorf("decoded %+v", m)
	}
	future := []byte(`{"t":"message","thread":"s:C:T","turn":"turn_1","channel":"C","user":{"id":"U","pronouns":"x"},"text":"hi","some_new_field":1}`)
	if _, err := protocol.Decode(future, protocol.DirDown); err != nil {
		t.Fatalf("unknown fields must be ignored, not rejected: %v", err)
	}
}
