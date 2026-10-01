package gateway

import (
	"fmt"
	"strings"

	"github.com/analystaio/splitscreen/internal/surface"
)

// contextHeader renders the one line the agent sees in front of every message:
//
//	[Slack #box-foo (C0123) · from Jane Doe <jane@example.com> (U0456)]
//
// Several channels and several people can share one runner and one thread, so
// the agent is told on every turn rather than once per session — otherwise it
// cannot credit the right person in a commit or answer the right question.
// Names are whatever the surface could resolve; ids are always present, so a
// missing scope degrades the header rather than dropping it.
func contextHeader(in surface.Inbound) string {
	where := "#" + clean(in.ChannelName) + " (" + in.Channel + ")"
	switch {
	case in.IsDM:
		where = "DM"
	case in.ChannelName == "":
		where = in.Channel
	}

	var who []string
	if d := clean(in.User.Display); d != "" {
		who = append(who, d)
	}
	if e := clean(in.User.Email); e != "" {
		who = append(who, "<"+e+">")
	}
	who = append(who, "("+in.User.ID+")")

	return fmt.Sprintf("[%s %s · from %s]", surfaceTitle(in.Surface), where, strings.Join(who, " "))
}

func surfaceTitle(name string) string {
	switch name {
	case "slack":
		return "Slack"
	case "":
		return "Chat"
	default:
		return name
	}
}

// clean keeps a resolved name to one short line. Names are user-controlled, and
// the header is prompt text: a newline or a closing bracket in a display name
// must not be able to end the header and start something that reads like an
// instruction.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '[', ']', '<', '>':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 64 {
		s = string([]rune(s)[:64]) + "…"
	}
	return s
}
