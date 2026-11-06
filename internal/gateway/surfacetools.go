package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/analystaio/splitscreen/internal/mcpproxy"
	"github.com/analystaio/splitscreen/internal/surface"
	"github.com/analystaio/splitscreen/protocol"
)

// Surface tools run inside the gateway, on the chat surface, with the credential
// the gateway already holds. To a runner they are a proxied MCP server, so the
// agent gets a narrow tool and never the token behind it. Calls land in the same
// audit table as every other MCP call, with the requesting human attached.

const createChannelTool = "create_channel"

// maxChannelInvites bounds one call. Slack's own limit is far higher; this is
// about what a mistaken or misled agent can do in a single step.
const maxChannelInvites = 20

var (
	channelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)
	userIDRe      = regexp.MustCompile(`^[UW][A-Z0-9]{2,}$`)
)

const surfaceToolList = `{"tools":[{
  "name":"create_channel",
  "description":"Create a chat channel. The person who asked is added automatically, and the bot joins it. Channels cannot be renamed or archived with this tool, so only create one when someone asked for it.",
  "inputSchema":{"type":"object","required":["name"],"additionalProperties":false,"properties":{
    "name":{"type":"string","description":"Lowercase letters, digits, hyphens and underscores, up to 80 characters, with no leading # and no spaces."},
    "private":{"type":"boolean","description":"Only invited people can see it. Defaults to false."},
    "invite":{"type":"array","maxItems":20,"items":{"type":"string"},"description":"User ids to add as well as the person who asked."}
  }}
}]}`

type createChannelArgs struct {
	Name    string   `json:"name"`
	Private bool     `json:"private"`
	Invite  []string `json:"invite"`
}

// callSurfaceTool runs one tool. Any error means nothing happened.
func (g *Gateway) callSurfaceTool(ctx context.Context, runner string, turn *turnContext, fr *protocol.MCPCall) (json.RawMessage, error) {
	if !g.runnerHasServer(runner, fr.Server) {
		return nil, fmt.Errorf("server %q is not in this runner's bundle", fr.Server)
	}
	switch fr.Tool {
	case mcpproxy.ListTool:
		return json.RawMessage(surfaceToolList), nil
	case createChannelTool:
		return g.createChannel(ctx, runner, turn, fr.Args)
	}
	return nil, fmt.Errorf("unknown tool %q on server %q", fr.Tool, fr.Server)
}

// runnerHasServer reports whether a runner's bundle lists a server. The agent
// reaches this through a socket it can write to, so a name it was never given
// must not work.
func (g *Gateway) runnerHasServer(runner, server string) bool {
	cfg := g.cfg.Load()
	rc, ok := cfg.Runners[runner]
	if !ok {
		return false
	}
	rb, err := cfg.ResolveBundle(rc.Bundle)
	return err == nil && slices.Contains(rb.MCP, server)
}

func (g *Gateway) createChannel(ctx context.Context, runner string, turn *turnContext, raw json.RawMessage) (json.RawMessage, error) {
	// A person has to be behind this, and it has to be this runner's turn: a
	// channel outlives the session, and "who asked" is what the audit is for.
	if turn == nil || turn.Runner != runner {
		return nil, errors.New("create_channel needs a running turn: ask from a chat thread")
	}

	var args createChannelArgs
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return nil, fmt.Errorf("create_channel: bad arguments: %w", err)
	}
	if !channelNameRe.MatchString(args.Name) {
		return nil, fmt.Errorf("create_channel: %q is not a valid name: use lowercase letters, digits, hyphens and underscores, up to 80 characters", args.Name)
	}
	if len(args.Invite) > maxChannelInvites {
		return nil, fmt.Errorf("create_channel: %d invites is over the limit of %d", len(args.Invite), maxChannelInvites)
	}
	for _, id := range args.Invite {
		if !userIDRe.MatchString(id) {
			return nil, fmt.Errorf("create_channel: %q is not a user id", id)
		}
	}

	srf := g.surfaces[turn.Surface]
	creator, ok := srf.(surface.ChannelCreator)
	if !ok {
		return nil, fmt.Errorf("create_channel: surface %q cannot create channels", turn.Surface)
	}
	// Separate from any route check. A route that names a channel may admit
	// guests on purpose; that is not a reason to let them add to the workspace.
	v, ok := srf.(surface.AudienceVerifier)
	if !ok || !v.SenderIsMember(ctx, turn.User.ID) {
		return nil, errors.New("create_channel: only a verified full member can create channels")
	}

	invite := []string{turn.User.ID}
	for _, id := range args.Invite {
		if !slices.Contains(invite, id) {
			invite = append(invite, id)
		}
	}
	ch, err := creator.CreateChannel(ctx, surface.ChannelRequest{
		Name: args.Name, Private: args.Private, Invite: invite,
	})
	if err != nil {
		return nil, err
	}
	g.log.Info("channel created", "runner", runner, "channel", ch.ID, "name", ch.Name,
		"private", args.Private, "user", turn.User.ID)
	return toolText(createdChannelText(ch, turn.User.ID)), nil
}

// createdChannelText tells the agent what happened, failures included, so it can
// tell the person rather than assume they were added.
func createdChannelText(ch surface.CreatedChannel, requester string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Created #%s (%s). Link it in chat as <#%s>.", ch.Name, ch.ID, ch.ID)
	if len(ch.InviteFailures) == 0 {
		return b.String()
	}
	users := make([]string, 0, len(ch.InviteFailures))
	for u := range ch.InviteFailures {
		users = append(users, u)
	}
	sort.Strings(users)
	b.WriteString(" The channel exists, so do not create it again, but these invites failed:")
	for _, u := range users {
		fmt.Fprintf(&b, " %s (%s)", u, ch.InviteFailures[u])
		if u == requester {
			b.WriteString(" [the person who asked]")
		}
	}
	return b.String()
}

// toolText wraps text as an MCP tool result.
func toolText(text string) json.RawMessage {
	out, _ := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	return out
}
