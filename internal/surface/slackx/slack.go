// Package slackx implements the Slack surface over Socket Mode.
//
// Exactly one connection per app exists, and it lives on the gateway. That is
// the whole point: Socket Mode load-balances across connections, so two
// processes sharing an app token receive each payload nondeterministically.
package slackx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// Surface is the Slack adapter.
type Surface struct {
	api    *slack.Client
	sock   *socketmode.Client
	botID  string
	selfID string
	teamID string

	// noStream latches when Slack says this app cannot stream, so the gateway
	// stops paying a failed round trip per turn to rediscover it.
	noStream atomic.Bool

	dir *directory

	mu     sync.RWMutex
	closed bool
}

// New builds the adapter. botToken is xoxb-, appToken is xapp- with
// connections:write.
func New(botToken, appToken string) (*Surface, error) {
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, errors.New("slack: bot token should start with xoxb-")
	}
	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, errors.New("slack: app token should start with xapp-")
	}
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	return &Surface{
		api:  api,
		sock: socketmode.New(api),
		dir:  newDirectory(api),
	}, nil
}

func (s *Surface) Name() string { return "slack" }

// Start connects and pumps events until ctx is cancelled.
func (s *Surface) Start(ctx context.Context, h surface.Handler) error {
	auth, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack: auth test: %w", err)
	}
	s.selfID = auth.UserID
	s.botID = auth.BotID
	// Streaming into a channel needs the recipient's team. For a Slack Connect
	// guest this is their home team, not ours; using the bot's team is right
	// for the single-workspace case and wrong nowhere it is currently used.
	s.teamID = auth.TeamID

	go s.pump(ctx, h)
	return s.sock.RunContext(ctx)
}

func (s *Surface) pump(ctx context.Context, h surface.Handler) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-s.sock.Events:
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				api, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				s.sock.Ack(*evt.Request)
				s.handleEvent(ctx, h, api)
			case socketmode.EventTypeInteractive:
				cb, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				s.sock.Ack(*evt.Request)
				s.handleInteraction(ctx, h, cb)
			}
		}
	}
}

func (s *Surface) handleEvent(ctx context.Context, h surface.Handler, api slackevents.EventsAPIEvent) {
	if api.Type != slackevents.CallbackEvent {
		return
	}
	ev, ok := api.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Ignore our own posts, and every edit/delete/join subtype. "file_share" is
	// deliberately allowed: it is how a message carrying an attachment arrives,
	// and dropping it would silently discard every upload.
	if ev.BotID != "" || ev.User == "" || ev.User == s.selfID {
		return
	}
	switch ev.SubType {
	case "", "file_share":
	default:
		return
	}

	thread := ev.ThreadTimeStamp
	if thread == "" {
		thread = ev.TimeStamp
	}

	in := surface.Inbound{
		Surface: s.Name(),
		Channel: ev.Channel,
		Thread:  thread,
		User:    surface.User{ID: ev.User},
		Text:    ev.Text,
		IsDM:    ev.ChannelType == "im",
	}
	// The library's custom unmarshaller populates Message for plain messages as
	// well as changed ones, and attachments only ever live there.
	if ev.Message != nil {
		for _, f := range ev.Message.Files {
			in.Files = append(in.Files, s.inboundFile(f))
		}
		if in.Text == "" {
			in.Text = ev.Message.Text
		}
	}
	// Computed after the text is final, because an upload carries its caption on
	// ev.Message rather than ev.Text — reading it too early makes "@bot look at
	// this" with a file attached indistinguishable from an unaddressed drop.
	in.Addressed = in.IsDM || strings.Contains(in.Text, "<@"+s.selfID+">")
	s.enrich(ctx, &in)
	h.OnMessage(ctx, in)
}

// enrich adds the sender's name and email and the channel's name, so the agent
// knows who is asking and where. Best effort: see directory.
func (s *Surface) enrich(ctx context.Context, in *surface.Inbound) {
	if s.dir == nil {
		return
	}
	in.User.Display, in.User.Email = s.dir.user(ctx, in.User.ID)
	if !in.IsDM {
		in.ChannelName = s.dir.channel(ctx, in.Channel)
	}
}

func (s *Surface) inboundFile(f slack.File) surface.File {
	// URLPrivateDownload needs the bot token, which lives here and nowhere else.
	url := f.URLPrivateDownload
	if url == "" {
		url = f.URLPrivate
	}
	return surface.File{
		Name: f.Name,
		Mime: f.Mimetype,
		Size: int64(f.Size),
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			pr, pw := io.Pipe()
			go func() {
				err := s.api.GetFileContext(ctx, url, pw)
				pw.CloseWithError(err)
			}()
			return pr, nil
		},
	}
}

// permissionAction is the action_id prefix for permission buttons. The request
// id rides in the button value so a decision needs no server-side lookup table.
const permissionAction = "splitscreen_permission"

func (s *Surface) handleInteraction(ctx context.Context, h surface.Handler, cb slack.InteractionCallback) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return
	}
	for _, a := range cb.ActionCallback.BlockActions {
		if !strings.HasPrefix(a.ActionID, permissionAction) {
			continue
		}
		requestID, decision, ok := strings.Cut(a.Value, "|")
		if !ok {
			continue
		}
		h.OnDecision(ctx, surface.Decision{
			RequestID: requestID,
			Decision:  protocol.Decision(decision),
			User:      surface.User{ID: cb.User.ID, Display: cb.User.Name},
			Ref: surface.Ref{
				Channel: cb.Channel.ID,
				Thread:  cb.Message.ThreadTimestamp,
				ID:      cb.Message.Timestamp,
			},
		})
	}
}

// ---------------------------------------------------------------------------
// Outbound
// ---------------------------------------------------------------------------

func personaOptions(p surface.Persona) []slack.MsgOption {
	var opts []slack.MsgOption
	if p.Name != "" {
		opts = append(opts, slack.MsgOptionUsername(p.Name))
	}
	if p.Icon != "" {
		if strings.HasPrefix(p.Icon, "http") {
			opts = append(opts, slack.MsgOptionIconURL(p.Icon))
		} else {
			opts = append(opts, slack.MsgOptionIconEmoji(p.Icon))
		}
	}
	return opts
}

func (s *Surface) Post(ctx context.Context, p surface.Post) (surface.Ref, error) {
	opts := []slack.MsgOption{slack.MsgOptionText(ToMrkdwn(p.Text), false)}
	if p.Thread != "" {
		opts = append(opts, slack.MsgOptionTS(p.Thread))
	}
	opts = append(opts, personaOptions(p.Persona)...)

	if p.Ephemeral && p.User != "" {
		ts, err := s.api.PostEphemeralContext(ctx, p.Channel, p.User, opts...)
		if err != nil {
			return surface.Ref{}, fmt.Errorf("slack: post ephemeral: %w", err)
		}
		return surface.Ref{Channel: p.Channel, Thread: p.Thread, ID: ts}, nil
	}

	channel, ts, err := s.api.PostMessageContext(ctx, p.Channel, opts...)
	if err != nil {
		return surface.Ref{}, fmt.Errorf("slack: post: %w", err)
	}
	return surface.Ref{Channel: channel, Thread: p.Thread, ID: ts}, nil
}

func (s *Surface) Update(ctx context.Context, ref surface.Ref, p surface.Post) error {
	opts := []slack.MsgOption{slack.MsgOptionText(ToMrkdwn(p.Text), false)}
	opts = append(opts, personaOptions(p.Persona)...)
	_, _, _, err := s.api.UpdateMessageContext(ctx, ref.Channel, ref.ID, opts...)
	if err != nil {
		return fmt.Errorf("slack: update: %w", err)
	}
	return nil
}

func (s *Surface) Prompt(ctx context.Context, p surface.Prompt) (surface.Ref, error) {
	header := fmt.Sprintf("*Permission requested* — `%s`", p.Tool)
	if p.Summary != "" {
		header += "\n" + ToMrkdwn(p.Summary)
	}
	blocks := []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, header, false, false), nil, nil),
	}
	if p.Detail != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, "```\n"+truncate(p.Detail, 2500)+"\n```", false, false),
			nil, nil))
	}
	blocks = append(blocks, slack.NewActionBlock(
		permissionAction,
		// Three buttons, each doing exactly what it says. "Always" was removed:
		// it implied appending to a runner's allow list, which would mean a chat
		// click mutating a reviewed config file. Use `splitscreen policy allow`
		// for that, so the change leaves a diff.
		button("Allow", string(protocol.DecisionAllow), p.RequestID, slack.StylePrimary),
		button("Session", string(protocol.DecisionAllowSession), p.RequestID, slack.StyleDefault),
		button("Deny", string(protocol.DecisionDeny), p.RequestID, slack.StyleDanger),
	))

	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
		slack.MsgOptionText(fmt.Sprintf("Permission requested: %s", p.Tool), false),
	}
	if p.Thread != "" {
		opts = append(opts, slack.MsgOptionTS(p.Thread))
	}
	opts = append(opts, personaOptions(p.Persona)...)

	channel, ts, err := s.api.PostMessageContext(ctx, p.Channel, opts...)
	if err != nil {
		return surface.Ref{}, fmt.Errorf("slack: prompt: %w", err)
	}
	return surface.Ref{Channel: channel, Thread: p.Thread, ID: ts}, nil
}

func button(label, decision, requestID string, style slack.Style) *slack.ButtonBlockElement {
	b := slack.NewButtonBlockElement(
		permissionAction+"_"+decision,
		requestID+"|"+decision,
		slack.NewTextBlockObject(slack.PlainTextType, label, false, false),
	)
	if style != slack.StyleDefault {
		b.Style = style
	}
	return b
}

// Resolve strips the buttons from a decided prompt. Leaving live controls on a
// resolved request invites a second click that can never take effect.
func (s *Surface) Resolve(ctx context.Context, ref surface.Ref, text string) error {
	text = ToMrkdwn(text)
	_, _, _, err := s.api.UpdateMessageContext(ctx, ref.Channel, ref.ID,
		slack.MsgOptionBlocks(slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)),
		slack.MsgOptionText(text, false),
	)
	if err != nil {
		return fmt.Errorf("slack: resolve prompt: %w", err)
	}
	return nil
}

func (s *Surface) Upload(ctx context.Context, u surface.Upload) error {
	// UploadFileContext performs the three-step external upload flow; the old
	// files.upload endpoint is retired.
	_, err := s.api.UploadFileContext(ctx, slack.UploadFileParameters{
		Channel:         u.Channel,
		Reader:          u.Content,
		Filename:        u.Name,
		FileSize:        int(u.Size),
		ThreadTimestamp: u.Thread,
		InitialComment:  u.Comment,
	})
	if err != nil {
		return fmt.Errorf("slack: upload: %w", err)
	}
	return nil
}

// SetStatus shows or clears Slack's native working indicator in a thread
// ("<app> is working…"), via assistant.threads.setStatus. It needs only
// chat:write and works in ordinary channel threads, not just assistant DMs.
// Slack clears it on its own after two minutes without a message, and whenever
// the app posts in the thread; the gateway refreshes it for as long as a turn
// runs.
func (s *Surface) SetStatus(ctx context.Context, st surface.Status) error {
	p := slack.AssistantThreadsSetStatusParameters{
		ChannelID: st.Channel,
		ThreadTS:  st.Thread,
		Status:    st.Text,
		Username:  st.Persona.Name,
	}
	if st.Persona.Icon != "" {
		if strings.HasPrefix(st.Persona.Icon, "http") {
			p.IconURL = st.Persona.Icon
		} else {
			p.IconEmoji = st.Persona.Icon
		}
	}
	err := s.api.SetAssistantThreadsStatusContext(ctx, p)
	if err == nil {
		return nil
	}
	var se slack.SlackErrorResponse
	if errors.As(err, &se) {
		switch se.Err {
		case "ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout":
			return err
		}
		return fmt.Errorf("%w: %s", surface.ErrStatusUnavailable, se.Err)
	}
	return err
}

// Channel reports membership for a routed channel.
//
// Slack only delivers message events for conversations the bot has joined, so a
// perfectly-routed channel nobody invited the bot into behaves exactly like an
// unrouted one. This is how that becomes visible.
func (s *Surface) Channel(ctx context.Context, id string) (surface.ChannelInfo, error) {
	info := surface.ChannelInfo{ID: id, Membership: surface.MembershipUnknown}

	ch, err := s.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID: id,
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "missing_scope"):
			// Report the gap rather than guessing. Without channels:read (and
			// groups:read for private channels) we simply cannot tell.
			info.Detail = "needs the channels:read scope (groups:read for private channels)"
		case strings.Contains(msg, "channel_not_found"):
			// Slack returns this both for a bad id and for a private channel the
			// bot cannot see, and the two are worth distinguishing by hand.
			info.Membership = surface.MembershipNotJoined
			info.Detail = "channel not found, or private and not joined"
		default:
			info.Detail = msg
		}
		return info, nil
	}

	info.Name = ch.Name
	if s.dir != nil {
		s.dir.rememberChannel(id, ch.Name)
	}
	if ch.IsMember {
		info.Membership = surface.MembershipJoined
	} else {
		info.Membership = surface.MembershipNotJoined
		info.Detail = "invite the bot into the channel"
	}
	return info, nil
}

func (s *Surface) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}

// RateLimitPause is how long to wait when Slack asks us to slow down.
// Consolidating every runner onto one app means one rate-limit bucket, so
// outbound is deliberately paced (see the gateway's stream coalescing).
const RateLimitPause = 2 * time.Second
