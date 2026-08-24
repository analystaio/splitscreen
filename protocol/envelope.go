package protocol

import (
	"encoding/json"
	"fmt"
)

// MaxControlFrame caps a single JSON control frame. Bulk payloads ride on
// binary frames (see blob.go), so any control frame near this size is a bug or
// an attack rather than legitimate traffic.
const MaxControlFrame = 1 << 20 // 1 MiB

// registry maps the wire discriminator to a constructor. Adding a frame type
// means adding exactly one line here.
var registry = map[FrameType]func() Frame{
	TypeHello:              func() Frame { return new(Hello) },
	TypeHelloAck:           func() Frame { return new(HelloAck) },
	TypeMessage:            func() Frame { return new(Message) },
	TypeBundlePush:         func() Frame { return new(BundlePush) },
	TypePermissionRequest:  func() Frame { return new(PermissionRequest) },
	TypePermissionResponse: func() Frame { return new(PermissionResponse) },
	TypeMCPCall:            func() Frame { return new(MCPCall) },
	TypeMCPResponse:        func() Frame { return new(MCPResponse) },
	TypeCredentialRequest:  func() Frame { return new(CredentialRequest) },
	TypeCredentialGrant:    func() Frame { return new(CredentialGrant) },
	TypeTextDelta:          func() Frame { return new(TextDelta) },
	TypeThought:            func() Frame { return new(Thought) },
	TypeToolStart:          func() Frame { return new(ToolStart) },
	TypeToolEnd:            func() Frame { return new(ToolEnd) },
	TypeUsage:              func() Frame { return new(Usage) },
	TypeDone:               func() Frame { return new(Done) },
	TypeError:              func() Frame { return new(Error) },
	TypeBlobBegin:          func() Frame { return new(BlobBegin) },
	TypeBlobEnd:            func() Frame { return new(BlobEnd) },
	TypePing:               func() Frame { return new(Ping) },
	TypePong:               func() Frame { return new(Pong) },
}

// KnownTypes returns every frame type this build understands, for status output
// and for tests that assert the registry and the constants stay in step.
func KnownTypes() []FrameType {
	out := make([]FrameType, 0, len(registry))
	for t := range registry {
		out = append(out, t)
	}
	return out
}

// Encode serializes a frame, injecting the "t" discriminator. Frames are
// validated on the way out as well as the way in: a peer should never have to
// diagnose our bugs.
func Encode(f Frame) ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("protocol: cannot encode nil frame")
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("protocol: refusing to encode invalid %s: %w", f.Type(), err)
	}
	body, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal %s: %w", f.Type(), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("protocol: %s did not marshal to an object: %w", f.Type(), err)
	}
	if _, taken := fields["t"]; taken {
		return nil, fmt.Errorf("protocol: %s defines a field named \"t\", which collides with the discriminator", f.Type())
	}
	disc, err := json.Marshal(f.Type())
	if err != nil {
		return nil, err
	}
	fields["t"] = disc
	return json.Marshal(fields)
}

// Decode parses a control frame received from the peer in direction `from`
// (DirUp when the gateway reads from a runner, DirDown when the runner reads
// from the gateway).
//
// Direction is enforced here rather than in handlers so that it cannot be
// forgotten at a call site: a runner must not be able to originate a
// permission.response and approve its own tool call.
func Decode(data []byte, from Direction) (Frame, error) {
	if from != DirUp && from != DirDown {
		return nil, fmt.Errorf("protocol: decode requires a concrete peer direction, got %v", from)
	}
	if len(data) > MaxControlFrame {
		return nil, fmt.Errorf("protocol: control frame of %d bytes exceeds cap %d", len(data), MaxControlFrame)
	}
	var probe struct {
		T FrameType `json:"t"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("protocol: malformed frame: %w", err)
	}
	if probe.T == "" {
		return nil, fmt.Errorf("protocol: frame is missing the \"t\" discriminator")
	}
	ctor, ok := registry[probe.T]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownFrame, probe.T)
	}
	f := ctor()
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("protocol: decode %s: %w", probe.T, err)
	}
	if !f.Direction().permits(from) {
		return nil, fmt.Errorf("%w: %s may only originate %s, received from %s peer",
			ErrWrongDirection, probe.T, f.Direction(), from)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("protocol: invalid %s: %w", probe.T, err)
	}
	return f, nil
}
