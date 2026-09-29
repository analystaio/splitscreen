// Package surface abstracts the chat platforms a gateway talks to.
//
// This is one of the two plug points in the system, symmetric with harness
// adapters on the other side. The gateway holds every surface credential;
// runners never call a chat API.
package surface

import (
	"context"
	"io"
	"time"

	"github.com/avarant/splitscreen/protocol"
)

// User is a person on a surface.
type User struct {
	ID      string
	Display string
	// Email is filled when the surface can read it; empty otherwise.
	Email string
}

// Ref identifies a posted message so it can be edited in place. Streaming
// output is an edit loop, not a series of posts.
type Ref struct {
	Channel string
	Thread  string
	ID      string
}

// Inbound is a normalized user message.
type Inbound struct {
	Surface string
	Channel string
	// Thread is the conversation key. Surfaces without native threads use the
	// channel id, which makes the whole channel one conversation.
	Thread string
	User   User
	Text   string
	IsDM   bool
	// Addressed reports that this message was aimed at the bot rather than at
	// the room: an @-mention, or a DM, where the surface can tell. It gates
	// starting a NEW conversation, so that a runner can live in a channel
	// people also talk to each other in. Continuing an existing thread does not
	// consult it — once a conversation is underway, replies are the reply.
	//
	// Surfaces with no notion of addressing leave this false and must instead
	// be routed to a channel dedicated to the bot.
	Addressed bool
	Files     []File
	// ChannelName is the human name of Channel ("box-foo"), when the surface
	// could resolve it. Empty for a DM, or when resolution failed.
	ChannelName string
}

// File is an attachment on an inbound message. Open is called at most once and
// the gateway is responsible for closing the reader.
type File struct {
	Name string
	Mime string
	Size int64
	Open func(context.Context) (io.ReadCloser, error)
}

// Decision is a permission prompt resolved by a human.
type Decision struct {
	RequestID string
	Decision  protocol.Decision
	User      User
	Ref       Ref
}

// Handler receives everything a surface produces. Implemented by the gateway.
type Handler interface {
	OnMessage(context.Context, Inbound)
	OnDecision(context.Context, Decision)
}

// Persona is the display identity a runner posts under. One bot user can wear
// many personas; the tradeoff is that a persona cannot be @-mentioned
// individually, which is why routing is channel-based.
type Persona struct {
	Name string
	Icon string
}

// Post is an outbound message.
type Post struct {
	Channel string
	Thread  string
	Text    string
	Persona Persona
	// Ephemeral messages are visible only to User, used for notices that would
	// be noise in a shared thread.
	Ephemeral bool
	User      string
}

// StepStatus is where one step of a turn has got to.
type StepStatus string

const (
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
)

// Step is one unit of work inside a turn — a tool call, almost always.
//
// This is deliberately not "a Slack task card". A surface that can render
// progress natively shows steps as they change; one that cannot folds them into
// text. The gateway describes what happened and never decides how it looks.
type Step struct {
	ID     string
	Title  string
	Detail string
	Status StepStatus
	// Output is filled on failure, and carries the error rather than the
	// result: a successful tool's output is the harness's business.
	Output  string
	Elapsed time.Duration
}

// StreamUpdate is one increment of a turn: the prose written since the last
// update, plus every step whose status changed. Sending deltas rather than the
// whole message is what lets a surface append instead of rewrite.
type StreamUpdate struct {
	Text  string
	Steps []Step
}

// Stream is a message built up as the turn runs.
//
// Append is called on the flush interval and Close exactly once. A Stream that
// fails mid-turn is not resumable — the gateway falls back to post-and-edit for
// the remainder rather than dropping output.
type Stream interface {
	Append(ctx context.Context, u StreamUpdate) error
	Close(ctx context.Context, u StreamUpdate) error
	Ref() Ref
}

// Streamer is implemented by surfaces with a native progressive-message API.
//
// It is optional on purpose: the gateway checks for it and falls back to
// posting a message and editing it, which is what every surface can do. Post
// carries the triggering user in User, because Slack requires a recipient when
// streaming into a channel.
type Streamer interface {
	OpenStream(ctx context.Context, p Post) (Stream, error)
}

// Prompt is a permission request rendered as interactive controls.
type Prompt struct {
	Channel   string
	Thread    string
	Persona   Persona
	RequestID string
	Tool      string
	Summary   string
	Detail    string
}

// Upload is a file sent from a runner to the surface.
type Upload struct {
	Channel string
	Thread  string
	Name    string
	Mime    string
	Comment string
	Content io.Reader
	Size    int64
}

// Membership is what a surface knows about the bot's access to a channel.
//
// Unknown is a distinct state on purpose. A surface that lacks the scope to
// answer must not report "not joined", because that would paint a healthy
// deployment red and teach operators to ignore the check.
type Membership int

const (
	MembershipUnknown Membership = iota
	MembershipJoined
	MembershipNotJoined
)

func (m Membership) String() string {
	switch m {
	case MembershipJoined:
		return "joined"
	case MembershipNotJoined:
		return "not joined"
	default:
		return "unknown"
	}
}

// ChannelInfo is what a surface can report about a routed channel.
type ChannelInfo struct {
	ID         string
	Name       string
	Membership Membership
	// Detail explains an Unknown result — a missing scope, usually — so the
	// operator can act on it rather than wonder.
	Detail string
}

// Surface is a chat platform adapter.
type Surface interface {
	// Name identifies the adapter ("slack"). Used in thread keys and audit rows.
	Name() string
	// Start connects and blocks until ctx is cancelled. Inbound traffic is
	// delivered to h.
	Start(ctx context.Context, h Handler) error
	// Post sends a new message and returns a ref for later edits.
	Post(ctx context.Context, p Post) (Ref, error)
	// Update edits a previously posted message in place.
	Update(ctx context.Context, ref Ref, p Post) error
	// Prompt posts an interactive permission request.
	Prompt(ctx context.Context, p Prompt) (Ref, error)
	// Resolve replaces a prompt with its outcome, so a thread does not keep
	// live buttons for an already-decided request.
	Resolve(ctx context.Context, ref Ref, text string) error
	// Upload sends a file.
	Upload(ctx context.Context, u Upload) error
	// Channel reports what the surface knows about a channel, so a route the
	// bot cannot actually receive from can be surfaced as a problem. Silence is
	// otherwise indistinguishable from having no route at all.
	Channel(ctx context.Context, id string) (ChannelInfo, error)
	// Close releases the connection.
	Close() error
}
