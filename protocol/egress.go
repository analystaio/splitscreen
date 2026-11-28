package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Egress: HTTP requests made with a secret the agent never sees
// ---------------------------------------------------------------------------
//
// The agent calls a loopback proxy on its runner. The runner forwards the
// request here, the gateway attaches the named secret and makes the real call,
// and the response comes back the same way. The secret never crosses to the
// runner. See docs/secrets-broker.md.

// MaxEgressBody caps a request or response body carried by an egress frame.
// Bodies ride inside the control frame (base64 in JSON, so about 4/3 of this),
// which must stay under MaxControlFrame. Larger bodies are refused with a
// clear error rather than truncated.
const MaxEgressBody = 512 << 10 // 512 KiB

// EgressRequest is one proxied HTTP request.
type EgressRequest struct {
	RequestID string `json:"request"`
	// Secret names the vault secret to attach.
	Secret string `json:"secret"`
	// Host picks one of the secret's allowed hosts. Empty means its first.
	Host   string `json:"host,omitempty"`
	Method string `json:"method"`
	// Path is the upstream path and query, starting with "/".
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`

	ThreadID string `json:"thread,omitempty"`
	TurnID   string `json:"turn,omitempty"`
	// Attributed means the runner traced the calling process to the harness
	// it started for ThreadID, and that the runner is hardened, so the agent
	// cannot have forged that answer. Without it the gateway treats the
	// request as coming from no particular person.
	Attributed bool `json:"attributed,omitempty"`
}

func (*EgressRequest) Type() FrameType      { return TypeEgressRequest }
func (*EgressRequest) Direction() Direction { return DirUp }

func (e *EgressRequest) Validate() error {
	var errs []error
	if e.RequestID == "" {
		errs = append(errs, errors.New("egress.request: request is required"))
	}
	if e.Secret == "" {
		errs = append(errs, errors.New("egress.request: secret is required"))
	}
	if e.Method == "" || strings.ToUpper(e.Method) != e.Method {
		errs = append(errs, fmt.Errorf("egress.request: method %q must be upper case", e.Method))
	}
	if !strings.HasPrefix(e.Path, "/") {
		errs = append(errs, errors.New("egress.request: path must start with /"))
	}
	if len(e.Body) > MaxEgressBody {
		errs = append(errs, fmt.Errorf("egress.request: body of %d bytes exceeds %d", len(e.Body), MaxEgressBody))
	}
	if e.Attributed && (e.ThreadID == "" || e.TurnID == "") {
		errs = append(errs, errors.New("egress.request: an attributed request names its thread and turn"))
	}
	return errors.Join(errs...)
}

// EgressResponse answers an EgressRequest: either the upstream's response, or
// Error when the gateway refused or could not make the call.
type EgressResponse struct {
	RequestID string              `json:"request"`
	Status    int                 `json:"status,omitempty"`
	Headers   map[string][]string `json:"headers,omitempty"`
	Body      []byte              `json:"body,omitempty"`
	Error     *RemoteError        `json:"error,omitempty"`
}

func (*EgressResponse) Type() FrameType      { return TypeEgressResponse }
func (*EgressResponse) Direction() Direction { return DirDown }

func (e *EgressResponse) Validate() error {
	var errs []error
	if e.RequestID == "" {
		errs = append(errs, errors.New("egress.response: request is required"))
	}
	if e.Error == nil && (e.Status < 100 || e.Status > 599) {
		errs = append(errs, fmt.Errorf("egress.response: status %d is not an HTTP status", e.Status))
	}
	if e.Error != nil && e.Status != 0 {
		errs = append(errs, errors.New("egress.response: an error carries no status"))
	}
	if len(e.Body) > MaxEgressBody {
		errs = append(errs, fmt.Errorf("egress.response: body of %d bytes exceeds %d", len(e.Body), MaxEgressBody))
	}
	return errors.Join(errs...)
}
