// Package wake starts stopped runner hosts.
//
// A host that stops itself when idle — a per-task machine, say — is as
// reachable as one that never sleeps if the gateway can start it when a message
// arrives: the message is queued, the host boots, its runner connects, and the
// queue drains. This package is the "start it" half; the gateway owns the
// queueing, the rate limit, and what the thread is told.
package wake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/smithy-go"
)

// Result describes what a start request did.
type Result struct {
	// PreviousState is the instance state the request found: "stopped" means
	// this call started it; "pending" or "running" mean it was already on its
	// way up and the call was a no-op.
	PreviousState string
	CurrentState  string
}

// Started reports whether the host was asleep and this request woke it.
func (r Result) Started() bool { return r.PreviousState == "stopped" }

// ErrStopping means the host is still shutting down. EC2 refuses to start an
// instance in that state; the caller should try again once it has stopped.
var ErrStopping = errors.New("wake: instance is still stopping")

// Starter starts a host. Implementations must be safe for concurrent use.
type Starter interface {
	Start(ctx context.Context, region, instanceID string) (Result, error)
}

// startAPI is the one EC2 call used, separated so tests can supply a fake.
type startAPI interface {
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
}

// EC2 starts instances with the host's own IAM identity. Clients are built on
// first use, per region, so a gateway with no wakeable runner never loads AWS
// configuration at all.
type EC2 struct {
	// DefaultRegion applies when a runner's wake block names none. Empty falls
	// back to the ambient AWS configuration.
	DefaultRegion string

	mu      sync.Mutex
	clients map[string]startAPI
	// newClient is overridable for tests.
	newClient func(ctx context.Context, region string) (startAPI, error)
}

// NewEC2 returns a starter backed by the EC2 API.
func NewEC2(defaultRegion string) *EC2 {
	return &EC2{DefaultRegion: defaultRegion, clients: map[string]startAPI{}, newClient: loadClient}
}

func loadClient(ctx context.Context, region string) (startAPI, error) {
	var opts []func(*awscfg.LoadOptions) error
	if region != "" {
		opts = append(opts, awscfg.WithRegion(region))
	}
	cfg, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("wake: load aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("wake: no AWS region configured")
	}
	return ec2.NewFromConfig(cfg), nil
}

func (e *EC2) client(ctx context.Context, region string) (startAPI, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.clients[region]; ok {
		return c, nil
	}
	c, err := e.newClient(ctx, region)
	if err != nil {
		return nil, err
	}
	e.clients[region] = c
	return c, nil
}

// Start issues StartInstances. Starting a running or pending instance is a
// harmless no-op in EC2, so callers need not check state first.
func (e *EC2) Start(ctx context.Context, region, instanceID string) (Result, error) {
	if region == "" {
		region = e.DefaultRegion
	}
	c, err := e.client(ctx, region)
	if err != nil {
		return Result{}, err
	}
	out, err := c.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case "IncorrectInstanceState":
				return Result{}, ErrStopping
			case "UnauthorizedOperation":
				return Result{}, fmt.Errorf("the gateway is not allowed to start %s (ec2:StartInstances denied)", instanceID)
			case "InvalidInstanceID.NotFound", "InvalidInstanceID.Malformed":
				return Result{}, fmt.Errorf("instance %s does not exist", instanceID)
			case "InsufficientInstanceCapacity":
				return Result{}, fmt.Errorf("AWS has no capacity for this instance type right now; try again shortly")
			}
			return Result{}, fmt.Errorf("start %s: %s", instanceID, apiErr.ErrorMessage())
		}
		return Result{}, fmt.Errorf("start %s: %w", instanceID, err)
	}
	var res Result
	for _, sc := range out.StartingInstances {
		if sc.PreviousState != nil {
			res.PreviousState = string(sc.PreviousState.Name)
		}
		if sc.CurrentState != nil {
			res.CurrentState = string(sc.CurrentState.Name)
		}
	}
	return res, nil
}
